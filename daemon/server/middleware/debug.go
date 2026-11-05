package middleware

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/containerd/log"
	"github.com/moby/moby/v2/daemon/server/httpstatus"
	"github.com/moby/moby/v2/daemon/server/httputils"
	"github.com/moby/moby/v2/pkg/ioutils"
	"github.com/sirupsen/logrus"
)

// DebugRequestMiddleware dumps the request to logger
func DebugRequestMiddleware(handler func(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error) func(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
	return func(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
		logger := log.G(ctx)

		// Use a variable for fields to prevent overhead of repeatedly
		// calling WithFields.
		fields := log.Fields{
			"module":      "api",
			"method":      r.Method,
			"request-url": r.RequestURI,
			"vars":        vars,
		}
		handleWithLogs := func(ctx context.Context, w http.ResponseWriter, r *http.Request, vars map[string]string) error {
			logger.WithFields(fields).Debugf("handling %s request", r.Method)
			err := handler(ctx, w, r, vars)
			if err != nil {
				// TODO(thaJeztah): unify this with Server.makeHTTPHandler, which also logs internal server errors as error-log. See https://github.com/moby/moby/pull/48740#discussion_r1816675574
				fields["error-response"] = err
				fields["status"] = httpstatus.FromError(err)
				logger.WithFields(fields).Debugf("error response for %s request", r.Method)
			}
			return err
		}

		if r.Method != http.MethodPost {
			return handleWithLogs(ctx, w, r, vars)
		}
		if err := httputils.CheckForJSON(r); err != nil {
			return handleWithLogs(ctx, w, r, vars)
		}
		maxBodySize := 4096 // 4KB
		if r.ContentLength > int64(maxBodySize) {
			return handleWithLogs(ctx, w, r, vars)
		}

		body := r.Body
		bufReader := bufio.NewReaderSize(body, maxBodySize)
		r.Body = ioutils.NewReadCloserWrapper(bufReader, func() error { return body.Close() })

		b, err := bufReader.Peek(maxBodySize)
		if err != io.EOF {
			// either there was an error reading, or the buffer is full (in which case the request is too large)
			return handleWithLogs(ctx, w, r, vars)
		}

		var postForm map[string]any
		if err := json.Unmarshal(b, &postForm); err == nil {
			maskSecretKeys(postForm)
			// TODO(thaJeztah): is there a better way to detect if we're using JSON-formatted logs?
			if _, ok := logger.Logger.Formatter.(*logrus.JSONFormatter); ok {
				fields["form-data"] = postForm
			} else {
				if data, err := json.Marshal(postForm); err != nil {
					fields["form-data"] = postForm
				} else {
					fields["form-data"] = string(data)
				}
			}
		}

		return handleWithLogs(ctx, w, r, vars)
	}
}

func maskSecretKeys(inp any) {
	if arr, ok := inp.([]any); ok {
		for _, f := range arr {
			maskSecretKeys(f)
		}
		return
	}

	if form, ok := inp.(map[string]any); ok {
		scrub := []string{
			// Registry credentials sent in the body of POST /auth requests
			// (registry.AuthConfig). These field names are only used by
			// credential-bearing structs, so they are scrubbed unconditionally,
			// as is the "password" field below.
			"auth",
			"identitytoken",
			"registrytoken",
			// Note: The Data field contains the base64-encoded secret in 'secret'
			// and 'config' create and update requests. Currently, no other POST
			// API endpoints use a data field, so we scrub this field unconditionally.
			// Change this handling to be conditional if a new endpoint is added
			// in future where this field should not be scrubbed.
			"data",
			"jointoken",
			"password",
			"secret",
			"signingcakey",
			"unlockkey",
		}
	loop0:
		for k, v := range form {
			for _, m := range scrub {
				if strings.EqualFold(m, k) {
					form[k] = "*****"
					continue loop0
				}
			}
			maskSecretKeys(v)
		}
	}
}
