package opentsdb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/StackExchange/tsaf/third_party/github.com/StackExchange/slog"
)

var l = log.New(os.Stdout, "", log.LstdFlags)

type ResponseSet []*Response

type Point float64

type Response struct {
	Metric        string           `json:"metric"`
	Tags          TagSet           `json:"tags"`
	AggregateTags []string         `json:"aggregateTags"`
	DPS           map[string]Point `json:"dps"`
}

type DataPoint struct {
	Metric    string      `json:"metric"`
	Timestamp int64       `json:"timestamp"`
	Value     interface{} `json:"value"`
	Tags      TagSet      `json:"tags"`
}

func (d *DataPoint) Telnet() string {
	m := ""
	d.clean()
	for k, v := range d.Tags {
		m += fmt.Sprintf(" %s=%s", k, v)
	}
	return fmt.Sprintf("put %s %d %v%s\n", d.Metric, d.Timestamp, d.Value, m)
}

func (m MultiDataPoint) Json() ([]byte, error) {
	var md MultiDataPoint
	for _, d := range m {
		err := d.clean()
		if err != nil {
			slog.Infoln(err, "Removing Datapoint", d)
			continue
		}
		md = append(md, d)
	}
	return json.Marshal(md)
}

type MultiDataPoint []*DataPoint

type TagSet map[string]string

func (t TagSet) Equal(o TagSet) bool {
	if len(t) != len(o) {
		return false
	}
	for k, v := range t {
		if ov, ok := o[k]; !ok || ov != v {
			return false
		}
	}
	return true
}

// Subset returns true if all k=v pairs in o are in t.
func (t TagSet) Subset(o TagSet) bool {
	for k, v := range o {
		if tv, ok := t[k]; !ok || tv != v {
			return false
		}
	}
	return true
}

// String converts t to an OpenTSDB-style {a=b,c=b} string, alphabetized by key.
func (t TagSet) String() string {
	return fmt.Sprintf("{%s}", t.Tags())
}

// Tags is identical to String() but without { and }.
func (t TagSet) Tags() string {
	var keys []string
	for k := range t {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	b := &bytes.Buffer{}
	for i, k := range keys {
		if i > 0 {
			fmt.Fprint(b, ",")
		}
		fmt.Fprintf(b, "%s=%s", k, t[k])
	}
	return b.String()
}

func (d *DataPoint) clean() error {
	err := d.Tags.clean()
	if err != nil {
		return err
	}
	om := d.Metric
	d.Metric, err = Clean(d.Metric)
	if err != nil {
		return fmt.Errorf("%s. Orginal: [%s] Cleaned: [%s]", err.Error(), om, d.Metric)
	}
	if sv, ok := d.Value.(string); ok {
		if i, err := strconv.ParseInt(sv, 10, 64); err == nil {
			d.Value = i
		} else if f, err := strconv.ParseFloat(sv, 64); err == nil {
			d.Value = f
		} else {
			return fmt.Errorf("Unparseable number %v", sv)
		}
	}
	return nil
}

func (t TagSet) clean() error {
	for k, v := range t {
		kc, err := Clean(k)
		if err != nil {
			return fmt.Errorf("%s. Orginal: [%s] Cleaned: [%s]", err.Error(), k, kc)
		}
		vc, err := Clean(v)
		if err != nil {
			return fmt.Errorf("%s. Orginal: [%s] Cleaned: [%s]", err.Error(), v, vc)
		}
		delete(t, k)
		t[kc] = vc
	}
	return nil
}

// Clean removes characters from s that are invalid for OpenTSDB metric and tag
// values.
// See: http://opentsdb.net/docs/build/html/user_guide/writing.html#metrics-and-tags
func Clean(s string) (string, error) {
	var c string
	if len(s) == 0 {
		// This one is perhaps better checked earlier in the pipeline, but since
		// it makes sense to check that the resulting cleaned tag is not Zero length here I'm including it
		// It also might be the case that this just shouldn't be happening and this is yet another side
		// effect of WMI turning to Garbage....
		return s, errors.New("Metric/Tagk/Tagv Cleaning Passed a Zero Length String")
	}
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' || r == '/' {
			c += string(r)
		}
		s = s[size:]
	}

	if len(c) == 0 {
		return c, errors.New("Cleaning Metric/Tagk/Tagv resulted in a Zero Length String")
	}
	return c, nil
}

type Request struct {
	Start             interface{} `json:"start"`
	End               interface{} `json:"end,omitempty"`
	Queries           []*Query    `json:"queries"`
	NoAnnotations     bool        `json:"noAnnotations,omitempty"`
	GlobalAnnotations bool        `json:"globalAnnotations,omitempty"`
	MsResolution      bool        `json:"msResolution,omitempty"`
	ShowTSUIDs        bool        `json:"showTSUIDs,omitempty"`
}

func RequestFromJSON(b []byte) (*Request, error) {
	var r Request
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if v, ok := r.Start.(float64); ok {
		r.Start = int64(v)
	}
	if v, ok := r.End.(float64); ok {
		r.End = int64(v)
	}
	return &r, nil
}

type Query struct {
	Aggregator  string      `json:"aggregator"`
	Metric      string      `json:"metric"`
	Rate        bool        `json:"rate,omitempty"`
	RateOptions RateOptions `json:"rateOptions,omitempty"`
	Downsample  string      `json:"downsample,omitempty"`
	Tags        TagSet      `json:"tags,omitempty"`
}

type RateOptions struct {
	Counter    bool  `json:"counter,omitempty"`
	CounterMax int64 `json:"counterMax,omitempty"`
	ResetValue int64 `json:"resetValue,omitempty"`
}

// ParsesRequest parses OpenTSDB requests of the form: start=1h-ago&m=avg:cpu.
func ParseRequest(req string) (*Request, error) {
	v, err := url.ParseQuery(req)
	if err != nil {
		return nil, err
	}
	r := Request{}
	if s := v.Get("start"); s == "" {
		return nil, fmt.Errorf("tsdb: missing start: %s", req)
	} else {
		r.Start = s
	}
	for _, m := range v["m"] {
		q, err := ParseQuery(m)
		if err != nil {
			return nil, err
		}
		r.Queries = append(r.Queries, q)
	}
	if len(r.Queries) == 0 {
		return nil, fmt.Errorf("tsdb: missing m: %s", req)
	}
	return &r, nil
}

var qRE = regexp.MustCompile(`^(\w+):(?:(\w+-\w+):)?(?:(rate.*):)?([\w./]+)(?:\{([\w./,=*-|]+)\})?$`)

// ParseQuery parses OpenTSDB queries of the form: avg:rate:cpu{k=v}.
func ParseQuery(query string) (*Query, error) {
	q := Query{}
	m := qRE.FindStringSubmatch(query)
	if m == nil {
		return nil, fmt.Errorf("tsdb: bad query format: %s", query)
	}
	q.Aggregator = m[1]
	q.Downsample = m[2]
	q.Rate = strings.HasPrefix(m[3], "rate")
	var err error
	if q.Rate {
		sp := strings.Split(m[3], ",")
		q.RateOptions.Counter = len(sp) > 1
		if len(sp) > 2 {
			if sp[2] != "" {
				if q.RateOptions.CounterMax, err = strconv.ParseInt(sp[2], 10, 64); err != nil {
					return nil, err
				}
			}
		}
		if len(sp) > 3 {
			if q.RateOptions.ResetValue, err = strconv.ParseInt(sp[3], 10, 64); err != nil {
				return nil, err
			}
		}
	}
	q.Metric = m[4]
	if m[5] != "" {
		tags, err := ParseTags(m[5])
		if err != nil {
			return nil, err
		}
		q.Tags = tags
	}
	return &q, nil
}

// ParseTags parses OpenTSDB tagk=tagv pairs of the form: k=v,m=o.
func ParseTags(t string) (TagSet, error) {
	ts := make(TagSet)
	for _, v := range strings.Split(t, ",") {
		sp := strings.SplitN(v, "=", 2)
		if len(sp) != 2 {
			return nil, fmt.Errorf("tsdb: bad tag: %s", v)
		}
		sp[0] = strings.TrimSpace(sp[0])
		sp[1] = strings.TrimSpace(sp[1])
		if _, present := ts[sp[0]]; present {
			return nil, fmt.Errorf("tsdb: duplicated tag: %s", v)
		}
		ts[sp[0]] = sp[1]
	}
	return ts, nil
}

func (q Query) String() string {
	s := q.Aggregator + ":"
	if q.Downsample != "" {
		s += q.Downsample + ":"
	}
	if q.Rate {
		s += "rate:"
	}
	s += q.Metric
	if len(q.Tags) > 0 {
		s += "{"
		first := true
		for k, v := range q.Tags {
			if first {
				first = false
			} else {
				s += ","
			}
			s += k + "=" + v
		}
		s += "}"
	}
	return s
}

func (r Request) String() string {
	v := make(url.Values)
	for _, q := range r.Queries {
		v.Add("m", q.String())
	}
	v.Add("start", fmt.Sprint(r.Start))
	if e := fmt.Sprint(r.End); r.End != nil && e != "" {
		v.Add("end", e)
	}
	return v.Encode()
}

// ParseAbsTime returns the time of s, which must be of any non-relative (not
// "X-ago") format supported by OpenTSDB.
func ParseAbsTime(s string) (time.Time, error) {
	var t time.Time
	t_formats := [4]string{
		"2006/01/02-15:04:05",
		"2006/01/02-15:04",
		"2006/01/02-15",
		"2006/01/02",
	}
	for _, f := range t_formats {
		if t, err := time.Parse(f, s); err == nil {
			return t, nil
		}
	}
	i, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return t, err
	}
	return time.Unix(i, 0), nil
}

// ParseTime returns the time of v, which can be of any format supported by
// OpenTSDB.
func ParseTime(v interface{}) (time.Time, error) {
	now := time.Now().UTC()
	switch i := v.(type) {
	case string:
		if i != "" {
			if strings.HasSuffix(i, "-ago") {
				s := strings.TrimSuffix(i, "-ago")
				d, err := ParseDuration(s)
				if err != nil {
					return now, err
				}
				return now.Add(time.Duration(-d)), nil
			} else {
				return ParseAbsTime(i)
			}
		} else {
			return now, nil
		}
	case int64:
		return time.Unix(i, 0), nil
	default:
		return time.Time{}, errors.New("type must be string or int64")
	}
}

// GetDuration returns the duration from the request's start to end.
func GetDuration(r *Request) (Duration, error) {
	var t Duration
	if v, ok := r.Start.(string); ok && v == "" {
		return t, errors.New("start time must be provided")
	}
	start, err := ParseTime(r.Start)
	if err != nil {
		return t, err
	}
	var end time.Time
	if r.End != nil {
		end, err = ParseTime(r.End)
		if err != nil {
			return t, err
		}
	} else {
		end = time.Now()
	}
	t = Duration(end.Sub(start))
	return t, nil
}

// AutoDownsample sets the avg downsample aggregator to produce l points.
func (r *Request) AutoDownsample(l int64) error {
	if l == 0 {
		return errors.New("tsaf: target length must be > 0")
	}
	cd, err := GetDuration(r)
	if err != nil {
		return err
	}
	d := cd / Duration(l)
	if d < Duration(time.Second)*15 {
		return nil
	}
	ds := fmt.Sprintf("%ds-avg", int64(d.Seconds()))
	for _, q := range r.Queries {
		q.Downsample = ds
	}
	return nil
}

// Query performs a v2 OpenTSDB request to the given host. host should be of the
// form hostname:port. Can return a RequestError.
func (r Request) Query(host string) (ResponseSet, error) {
	u := url.URL{
		Scheme: "http",
		Host:   host,
		Path:   "/api/query",
	}
	b, err := json.Marshal(&r)
	if err != nil {
		return nil, err
	}
	resp, err := http.Post(u.String(), "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e := RequestError{Request: string(b)}
		b, _ = ioutil.ReadAll(resp.Body)
		if err := json.Unmarshal(b, &e); err == nil {
			return nil, &e
		}
		return nil, fmt.Errorf("tsdb: %s", b)
	}
	b, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var tr ResponseSet
	if err := json.Unmarshal(b, &tr); err != nil {
		return nil, err
	}
	return tr, nil
}

type RequestError struct {
	Request string
	Err     struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Details string `json:"details"`
	} `json:"error"`
}

func (r *RequestError) Error() string {
	return fmt.Sprintf("tsdb: %s: %s", r.Request, r.Err.Message)
}

type Context interface {
	Query(Request) (ResponseSet, error)
}

type Host string

func (h Host) Query(r Request) (ResponseSet, error) {
	return r.Query(string(h))
}

type Cache struct {
	host  string
	cache map[string]*cacheResult
}

type cacheResult struct {
	ResponseSet
	Err error
}

func NewCache(host string) *Cache {
	return &Cache{
		host:  host,
		cache: make(map[string]*cacheResult),
	}
}

func (c *Cache) Query(r Request) (ResponseSet, error) {
	b, err := json.Marshal(&r)
	if err != nil {
		return nil, err
	}
	s := string(b)
	if v, ok := c.cache[s]; ok {
		return v.ResponseSet, v.Err
	}
	rs, e := r.Query(c.host)
	c.cache[s] = &cacheResult{rs, e}
	return rs, e
}

type DateCache struct {
	*Cache
	now time.Time
}

func NewDateCache(host string, now time.Time) *DateCache {
	return &DateCache{
		Cache: NewCache(host),
		now:   now,
	}
}

func (c *DateCache) Query(r Request) (ResponseSet, error) {
	start, err := ParseTime(r.Start)
	if err != nil {
		return nil, err
	}
	var end time.Time
	if r.End != nil {
		end, err = ParseTime(r.End)
		if err != nil {
			return nil, err
		}
	} else {
		end = time.Now()
	}
	diff := c.now.Sub(end)
	start = start.Add(diff)
	end = end.Add(diff)
	r.Start = start.Unix()
	r.End = end.Unix()
	return c.Cache.Query(r)
}