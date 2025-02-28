package runner

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cheekybits/m"
	"github.com/matryer/silk/parse"
)

const indent = " "

// T represents types to which failures may be reported.
// The testing.T type is one such example.
type T interface {
	FailNow()
	Log(...interface{})
}

// Runner runs parsed tests.
type Runner struct {
	t       T
	rootURL string
	// RoundTripper is the transport to use when making requests.
	// By default it is http.DefaultTransport.
	RoundTripper http.RoundTripper
	// ParseBody is the function to use to attempt to parse
	// response bodies to make data avaialble for assertions.
	ParseBody func(r io.Reader) (interface{}, error)
	// Log is the function to log to.
	Log func(string)
	// Verbose is the function that logs verbose debug information.
	Verbose func(...interface{})
	// NewRequest makes a new http.Request. By default, uses http.NewRequest.
	NewRequest func(method, urlStr string, body io.Reader) (*http.Request, error)
}

// New makes a new Runner with the given testing T target and the
// root URL.
func New(t T, URL string) *Runner {
	return &Runner{
		t:            t,
		rootURL:      URL,
		RoundTripper: http.DefaultTransport,
		Log: func(s string) {
			fmt.Println(s)
		},
		Verbose: func(args ...interface{}) {
			if !testing.Verbose() {
				return
			}
			fmt.Println(args...)
		},
		ParseBody:  ParseJSONBody,
		NewRequest: http.NewRequest,
	}
}

func (r *Runner) log(args ...interface{}) {
	var strs []string
	for _, arg := range args {
		strs = append(strs, fmt.Sprint(arg))
	}
	strs = append(strs, " ")
	r.Log(strings.Join(strs, " "))
}

// RunGlob is a helper that runs the files returned by filepath.Glob.
//     runner.RunGlob(filepath.Glob("pattern"))
func (r *Runner) RunGlob(files []string, err error) {
	if err != nil {
		r.t.Log("silk:", err)
		r.t.FailNow()
		return
	}
	r.RunFile(files...)
}

// RunFile parses and runs the specified file(s).
func (r *Runner) RunFile(filenames ...string) {
	groups, err := parse.ParseFile(filenames...)
	if err != nil {
		r.log(err)
		return
	}
	r.RunGroup(groups...)
}

// RunGroup runs a parse.Group.
// Consider RunFile instead.
func (r *Runner) RunGroup(groups ...*parse.Group) {
	for _, group := range groups {
		r.runGroup(group)
	}
}

func (r *Runner) runGroup(group *parse.Group) {
	//r.log("===", group.Filename+":", string(group.Title))
	for _, req := range group.Requests {
		r.runRequest(group, req)
	}
}

func (r *Runner) runRequest(group *parse.Group, req *parse.Request) {
	m := string(req.Method)
	p := string(req.Path)
	var body io.Reader
	if len(req.Body) > 0 {
		body = req.Body.Reader()
	}

	absPath := r.rootURL + p
	r.Verbose(string(req.Method), absPath)

	// make request
	httpReq, err := r.NewRequest(m, absPath, body)
	if err != nil {
		r.log("invalid request: ", err)
		r.t.FailNow()
		return
	}
	// set body
	bodyLen := len(req.Body.String())
	httpReq.Header.Add("Content-Length", strconv.Itoa(bodyLen))
	r.Verbose(indent, "Content-Length:", bodyLen)
	// set request headers
	for _, line := range req.Details {
		detail := line.Detail()
		r.Verbose(indent, detail.String())
		httpReq.Header.Add(detail.Key, fmt.Sprintf("%v", detail.Value.Data))
	}
	// set parameters
	q := httpReq.URL.Query()
	for _, line := range req.Params {
		detail := line.Detail()
		r.Verbose(indent, detail.String())
		q.Add(detail.Key, fmt.Sprintf("%v", detail.Value.Data))
	}
	httpReq.URL.RawQuery = q.Encode()

	// perform request
	httpRes, err := r.RoundTripper.RoundTrip(httpReq)
	if err != nil {
		r.log(err)
		r.t.FailNow()
		return
	}
	defer httpRes.Body.Close()

	// collect response details
	responseDetails := make(map[string]interface{})
	for k, vs := range httpRes.Header {
		for _, v := range vs {
			responseDetails[k] = v
		}
	}

	// set other details
	responseDetails["Status"] = float64(httpRes.StatusCode)

	actualBody, err := ioutil.ReadAll(httpRes.Body)
	if err != nil {
		r.log("failed to read body: ", err)
		r.t.FailNow()
		return
	}

	// assert the body
	if len(req.ExpectedBody) > 0 {
		// check body against expected body
		if !r.assertBody(actualBody, req.ExpectedBody.Join()) {
			r.fail(group, req, req.ExpectedBody.Number(), "- body doesn't match")
			return
		}
	}

	// assert the details
	var parseDataOnce sync.Once
	var data interface{}
	var errData error
	if len(req.ExpectedDetails) > 0 {
		for _, line := range req.ExpectedDetails {
			detail := line.Detail()
			if strings.HasPrefix(detail.Key, "Data") {
				parseDataOnce.Do(func() {
					data, errData = r.ParseBody(bytes.NewReader(actualBody))
				})
				if !r.assertData(data, errData, detail.Key, detail.Value) {
					r.fail(group, req, line.Number, "- "+detail.Key+" doesn't match")
					return
				}
				continue
			}
			var actual interface{}
			var present bool
			if actual, present = responseDetails[detail.Key]; !present {
				r.log(detail.Key, fmt.Sprintf("expected %s: %s  actual %T: %s", detail.Value.Type(), detail, actual, "(missing)"))
				r.fail(group, req, line.Number, "- "+detail.Key+" doesn't match")
				return
			}
			if !r.assertDetail(detail.Key, actual, detail.Value) {
				r.fail(group, req, line.Number, "- "+detail.Key+" doesn't match")
				return
			}
		}
	}

}

func (r *Runner) fail(group *parse.Group, req *parse.Request, line int, args ...interface{}) {
	logargs := []interface{}{"--- FAIL:", string(req.Method), string(req.Path), "\n", group.Filename + ":" + strconv.FormatInt(int64(line), 10)}
	r.log(append(logargs, args...)...)
	r.t.FailNow()
}

func (r *Runner) assertBody(actual, expected []byte) bool {
	if !reflect.DeepEqual(actual, expected) {
		r.log("body expected:")
		r.log("```")
		r.log(string(expected))
		r.log("```")
		r.log("actual:")
		r.log("```")
		r.log(string(actual))
		r.log("```")
		return false
	}
	return true
}

func (r *Runner) assertDetail(key string, actual interface{}, expected *parse.Value) bool {
	if actual != expected.Data {
		actualVal := parse.ParseValue([]byte(fmt.Sprintf("%v", actual)))
		r.log(key, fmt.Sprintf("expected %s: %s  actual %T: %s", expected.Type(), expected, actual, actualVal))
		return false
	}
	return true
}

func (r *Runner) assertData(data interface{}, errData error, key string, expected *parse.Value) bool {
	if errData != nil {
		r.log(key, fmt.Sprintf("expected %s: %s  actual: failed to parse body: %s", expected.Type(), expected, errData))
		return false
	}
	if data == nil {
		r.log(key, fmt.Sprintf("expected %s: %s  actual: no data", expected.Type(), expected))
		return false
	}
	actual, ok := m.GetOK(map[string]interface{}{"Data": data}, key)
	if !ok && expected.Data != nil {
		r.log(key, fmt.Sprintf("expected %s: %s  actual: (missing)", expected.Type(), expected))
		return false
	}
	if !ok && expected.Data == nil {
		return true
	}
	if !expected.Equal(actual) {
		actualVal := parse.ParseValue([]byte(fmt.Sprintf("%v", actual)))
		r.log(key, fmt.Sprintf("expected %s: %s  actual %T: %s", expected.Type(), expected, actual, actualVal))
		return false
	}
	return true
}
