package request

import (
	"bytes"
	"encoding/json"
	"io"
	"io/ioutil"
	"net/http"
	"strings"
)

// Options defines request options, like request modifiers and which host to target
type Options struct {
	host             string
	requestModifiers []func(*http.Request) error
}

// Host creates a modifier that sets the specified host as the request URL host
func Host(host string) func(*Options) {
	return func(o *Options) {
		o.host = host
	}
}

// With adds a request modifier to the options
func With(f func(*http.Request) error) func(*Options) {
	return func(o *Options) {
		o.requestModifiers = append(o.requestModifiers, f)
	}
}

// Method creates a modifier that sets the specified string as the request method
func Method(method string) func(*Options) {
	return With(func(req *http.Request) error {
		req.Method = method
		return nil
	})
}

// RawString sets the specified string as body for the request
func RawString(content string) func(*Options) {
	return RawContent(ioutil.NopCloser(strings.NewReader(content)))
}

// RawContent sets the specified reader as body for the request
func RawContent(reader io.ReadCloser) func(*Options) {
	return With(func(req *http.Request) error {
		req.Body = reader
		return nil
	})
}

// ContentType sets the specified Content-Type request header
func ContentType(contentType string) func(*Options) {
	return With(func(req *http.Request) error {
		req.Header.Set("Content-Type", contentType)
		return nil
	})
}

// JSON sets the Content-Type request header to json
func JSON(o *Options) {
	ContentType("application/json")(o)
}

// JSONBody creates a modifier that encodes the specified data to a JSON string and set it as request body. It also sets
// the Content-Type header of the request.
func JSONBody(data interface{}) func(*Options) {
	return With(func(req *http.Request) error {
		jsonData := bytes.NewBuffer(nil)
		if err := json.NewEncoder(jsonData).Encode(data); err != nil {
			return err
		}
		req.Body = ioutil.NopCloser(jsonData)
		req.Header.Set("Content-Type", "application/json")
		return nil
	})
}
