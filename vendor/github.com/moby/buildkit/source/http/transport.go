package http

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/upload"
	"github.com/pkg/errors"
)

func newTransport(rt http.RoundTripper, sm *session.Manager, id string) http.RoundTripper {
	return &sessionHandler{rt: rt, sm: sm, id: id}
}

type sessionHandler struct {
	sm *session.Manager
	rt http.RoundTripper
	id string
}

func (h *sessionHandler) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != "buildkit-session" {
		return h.rt.RoundTrip(req)
	}

	if req.Method != "GET" {
		return nil, errors.Errorf("invalid request")
	}

	timeoutCtx, cancel := context.WithTimeout(context.TODO(), 5*time.Second)
	defer cancel()

	caller, err := h.sm.Get(timeoutCtx, h.id)
	if err != nil {
		return nil, err
	}

	up, err := upload.New(context.TODO(), caller, req.URL)
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()
	go func() {
		_, err := up.WriteTo(pw)
		pw.CloseWithError(err)
	}()

	resp := &http.Response{
		Status:        "200 OK",
		StatusCode:    200,
		Body:          pr,
		ContentLength: -1,
	}

	return resp, nil
}
