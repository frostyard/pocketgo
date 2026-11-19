package pocketdtest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
)

// Hosts is an http.RoundTripper that serves each request in memory with the
// handler of its URL's host ("host:port"), so the tests open no socket. A
// response streams as the handler writes it, as an event stream needs; the
// handler's request context ends when the client cancels the request or
// closes the body. A host without a handler fails as a refused connection
// would.
type Hosts map[string]http.Handler

// ErrNoHost is what a request to a host of no handler fails with.
var ErrNoHost = errors.New("pocketdtest: connection refused")

func (h Hosts) RoundTrip(r *http.Request) (*http.Response, error) {
	handler := h[r.URL.Host]
	if handler == nil {
		return nil, ErrNoHost
	}
	ctx, cancel := context.WithCancel(r.Context())
	pr, pw := io.Pipe()
	w := &pipeWriter{header: http.Header{}, pw: pw, ready: make(chan struct{})}
	served := r.Clone(ctx)
	if served.Body == nil {
		served.Body = http.NoBody // as a server's request has
	}
	go func() {
		defer func() {
			w.WriteHeader(http.StatusOK)
			_ = pw.Close()
			if r.Body != nil {
				_ = r.Body.Close()
			}
		}()
		handler.ServeHTTP(w, served)
	}()
	select {
	case <-w.ready:
	case <-ctx.Done():
		cancel()
		_ = pr.Close()
		return nil, ctx.Err()
	}
	return &http.Response{
		Status: http.StatusText(w.status), StatusCode: w.status, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: w.header, Body: &pipeBody{PipeReader: pr, cancel: cancel}, ContentLength: -1, Request: r,
	}, nil
}

// pipeWriter is the handler's http.ResponseWriter: its header is sent when
// the handler writes it, and its body goes through a pipe to the client.
type pipeWriter struct {
	header http.Header
	pw     *io.PipeWriter
	once   sync.Once
	status int
	ready  chan struct{}
}

func (w *pipeWriter) Header() http.Header { return w.header }

func (w *pipeWriter) WriteHeader(status int) {
	w.once.Do(func() {
		w.status = status
		close(w.ready)
	})
}

func (w *pipeWriter) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.pw.Write(b)
}

// Flush does nothing: a pipe hands each write to the reader as it comes.
func (w *pipeWriter) Flush() {}

// pipeBody ends the handler's request when the client closes the body.
type pipeBody struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (b *pipeBody) Close() error {
	b.cancel()
	return b.PipeReader.Close()
}
