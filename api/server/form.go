package server

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	restful "github.com/emicklei/go-restful"
)

func boolValue(r *http.Request, k string) bool {
	s := strings.ToLower(strings.TrimSpace(r.FormValue(k)))
	return !(s == "" || s == "0" || s == "no" || s == "false" || s == "none")
}

// boolValueOrDefault returns the default bool passed if the query param is
// missing, otherwise it's just a proxy to boolValue above
func boolValueOrDefault(r *http.Request, k string, d bool) bool {
	if _, ok := r.Form[k]; !ok {
		return d
	}
	return boolValue(r, k)
}

func int64ValueOrZero(r *http.Request, k string) int64 {
	val, err := strconv.ParseInt(r.FormValue(k), 10, 64)
	if err != nil {
		return 0
	}
	return val
}

type archiveOptions struct {
	name string
	path string
}

func archiveFormValues(r *restful.Request) (archiveOptions, error) {
	if err := parseForm(r.Request); err != nil {
		return archiveOptions{}, err
	}

	name := r.PathParameter("name")
	path := filepath.FromSlash(r.Request.Form.Get("path"))

	switch {
	case name == "":
		return archiveOptions{}, fmt.Errorf("bad parameter: 'name' cannot be empty")
	case path == "":
		return archiveOptions{}, fmt.Errorf("bad parameter: 'path' cannot be empty")
	}

	return archiveOptions{name, path}, nil
}
