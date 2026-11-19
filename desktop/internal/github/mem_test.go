package github

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
)

// The desktop's tests open no socket (AGENTS.md): a fake GitHub serves its
// handler in memory, at a host of its own that memNet routes to.

// memNetwork is an http.RoundTripper over the fake servers, by host.
type memNetwork struct {
	mu    sync.Mutex
	hosts map[string]http.Handler
	n     int
}

var memNet = &memNetwork{hosts: map[string]http.Handler{}}

// memServer is a fake server in memory: URL is its base, and Close takes
// it off the network, as httptest.Server's does.
type memServer struct {
	URL  string
	host string
}

// newMemServer serves h at a new host in memory.
func newMemServer(h http.Handler) *memServer {
	memNet.mu.Lock()
	defer memNet.mu.Unlock()
	memNet.n++
	host := fmt.Sprintf("srv%d.github.test", memNet.n)
	memNet.hosts[host] = h
	return &memServer{URL: "http://" + host, host: host}
}

// Close takes the server off the network: a request to it fails as one to
// a closed server does.
func (s *memServer) Close() {
	memNet.mu.Lock()
	defer memNet.mu.Unlock()
	delete(memNet.hosts, s.host)
}

var errNoServer = errors.New("connection refused: no server in memory at this host")

func (m *memNetwork) RoundTrip(r *http.Request) (*http.Response, error) {
	m.mu.Lock()
	h := m.hosts[r.URL.Host]
	m.mu.Unlock()
	if h == nil {
		return nil, errNoServer
	}
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	rec := &dropRecorder{ResponseRecorder: httptest.NewRecorder()}
	h.ServeHTTP(rec, r)
	if rec.dropped {
		return nil, io.ErrUnexpectedEOF
	}
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// dropper is a writer whose connection a handler can drop, with no answer.
type dropper interface{ Drop() }

type dropRecorder struct {
	*httptest.ResponseRecorder
	dropped bool
}

func (d *dropRecorder) Drop() { d.dropped = true }

// The default transport refuses: a client a test does not put on memNet
// fails instead of reaching the network (Codex review of #24, round 1).
func init() { http.DefaultTransport = noNetwork{} }

type noNetwork struct{}

func (noNetwork) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("the desktop's tests open no socket: %s would reach the network", r.URL)
}
