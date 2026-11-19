package state

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// Link runs a client's event stream. It reads GET /v1/events and hands
// each event on; when the stream ends it waits as Backoff says and reads it
// again from the last event it handed on (Last-Event-ID), until its context
// ends or the service refuses the sign-in or the protocol version.
//
// Its Client must be made with HTTPClient, which tells the link when the
// stream opens.
type Link struct {
	Client  *proto.Client
	Backoff Backoff
	// After returns a channel that fires after d; nil is time.After.
	After func(d time.Duration) <-chan time.Time

	// The callbacks run on the link's goroutine, one at a time, in order.
	// Opened: the service answered the stream with 200, so what the
	// client shows may be stale and is fetched again; the run of failures
	// ends. Event: an event, in the stream's order. Dropped: the stream
	// ended with err, the failures-th failure in a row, and the link waits
	// wait before the next try. Each may be nil.
	Opened  func()
	Event   func(proto.Event)
	Dropped func(err error, failures int, wait time.Duration)
}

// Run reads the stream from lastEventID (0 for the newest event) until ctx
// ends, and returns ctx's error, or the error that ends the link for good
// (Fatal).
func (l *Link) Run(ctx context.Context, lastEventID int64) error {
	after := l.After
	if after == nil {
		after = time.After
	}
	last, failures := lastEventID, 0
	opened := func() {
		failures = 0
		if l.Opened != nil {
			l.Opened()
		}
	}
	for {
		attempt := context.WithValue(ctx, openedKey{}, opened)
		err := l.Client.Events(attempt, last, func(ev proto.Event) error {
			if l.Event != nil {
				l.Event(ev)
			}
			if ev.Seq != 0 {
				last = ev.Seq
			}
			return nil
		})
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if Fatal(err) {
			return err
		}
		failures++
		wait := l.Backoff.Delay(failures)
		if l.Dropped != nil {
			l.Dropped(err, failures, wait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-after(wait):
		}
	}
}

// Fatal reports whether err ends the link for good: the service refused
// the device token or the protocol version, or redirected the stream to
// another origin. Trying again cannot help.
func Fatal(err error) bool {
	if errors.Is(err, ErrRedirect) {
		return true
	}
	var pe *proto.Error
	return errors.As(err, &pe) && (pe.Code == proto.ErrorCodeUnauthorized || pe.Code == proto.ErrorCodeProtocol)
}

// Unauthorized reports whether err is the service refusing the device
// token (401).
func Unauthorized(err error) bool {
	var pe *proto.Error
	return errors.As(err, &pe) && pe.Code == proto.ErrorCodeUnauthorized
}

// openedKey carries the function a Link runs when its stream opens, in the
// context of the stream's request.
type openedKey struct{}

// ErrRedirect is what a request fails with when pocketd redirects it to
// another origin (SameOrigin), or to another method.
var ErrRedirect = errors.New("pocketd redirected the request")

// maxRedirects is how many same-origin redirects a request follows.
const maxRedirects = 5

// HTTPClient returns the HTTP client a proto.Client of a Link needs:
// without a time limit, which the stream needs (bound each call with its
// context), and telling the Link when the stream's request is answered
// with 200. Every request carries the device token, so it follows a
// redirect only within the service's origin (SameOrigin) and with its
// method, and fails with ErrRedirect otherwise. A nil base is
// DirectTransport.
func HTTPClient(base http.RoundTripper) *http.Client {
	if base == nil {
		base = DirectTransport()
	}
	return &http.Client{Transport: watchTransport{base: base}, CheckRedirect: checkRedirect}
}

// DirectTransport is http.DefaultTransport without a proxy: the token goes
// straight to the service, by the route the sign-in judged
// (NeedsHTTPWarning), never to a proxy that HTTP_PROXY or HTTPS_PROXY
// names. A *.ts.net service skips the plain-HTTP warning because it is
// reached through the tailnet, which a proxy would not be.
func DirectTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	return t
}

func checkRedirect(req *http.Request, via []*http.Request) error {
	if !SameOrigin(via[0].URL, req.URL) {
		return fmt.Errorf("%w: %s://%s", ErrRedirect, req.URL.Scheme, req.URL.Host)
	}
	// A 301, 302 or 303 turns a POST into a GET without its body: the
	// command would never reach pocketd, and the page's 200 would look
	// like its success.
	if req.Method != via[0].Method {
		return fmt.Errorf("%w: it turned %s into %s", ErrRedirect, via[0].Method, req.Method)
	}
	if len(via) >= maxRedirects {
		return fmt.Errorf("pocketd redirected the request %d times", len(via))
	}
	return nil
}

type watchTransport struct{ base http.RoundTripper }

func (t watchTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err == nil && resp.StatusCode == http.StatusOK {
		if opened, ok := r.Context().Value(openedKey{}).(func()); ok {
			opened()
		}
	}
	return resp, err
}
