package signins

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"
)

// fakeGitHub answers a device flow's requests in memory, with no socket:
// codes, the start's answer; polls, one answer a poll, in order, the last
// one again once they run out; user, the account's.
type fakeGitHub struct {
	t     *testing.T
	mu    sync.Mutex
	code  answer
	polls []answer
	user  answer
	// seen are the requests' paths, in order, and forms their forms.
	seen  []string
	forms []url.Values
	auths []string
}

type answer struct {
	status int
	body   string
	err    error
}

func ok(body string) answer { return answer{status: 200, body: body} }

const (
	codeAnswer = `{"device_code":"dev-code-secret","user_code":"WDJB-MJHT","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`
	pending    = `{"error":"authorization_pending"}`
	slowDown   = `{"error":"slow_down","interval":10}`
	denied     = `{"error":"access_denied","error_description":"The user has denied your application access."}`
	expired    = `{"error":"expired_token"}`
	granted    = `{"access_token":"gho_granted_secret","token_type":"bearer","scope":"read:user"}`
	userAnswer = `{"login":"octocat","id":1}`
)

func newFakeGitHub(t *testing.T, polls ...answer) *fakeGitHub {
	return &fakeGitHub{t: t, code: ok(codeAnswer), polls: polls, user: ok(userAnswer)}
}

func (g *fakeGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	var a answer
	if r.Body != nil {
		data, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(data))
		g.forms = append(g.forms, form)
	}
	g.seen = append(g.seen, r.Method+" "+r.URL.String())
	g.auths = append(g.auths, r.Header.Get("Authorization"))
	switch r.URL.String() {
	case "https://github.com/login/device/code":
		a = g.code
	case "https://github.com/login/oauth/access_token":
		if len(g.polls) == 0 {
			g.t.Error("a poll with no answer left")
			return nil, errors.New("no answer")
		}
		a = g.polls[0]
		if len(g.polls) > 1 {
			g.polls = g.polls[1:]
		}
	case "https://api.github.com/user":
		a = g.user
	default:
		g.t.Errorf("a request to %s", r.URL)
		return nil, errors.New("unknown")
	}
	if a.err != nil {
		return nil, a.err
	}
	return &http.Response{StatusCode: a.status, Body: io.NopCloser(bytes.NewBufferString(a.body)), Header: http.Header{}, Request: r}, nil
}

func (g *fakeGitHub) client() *GitHub { return &GitHub{HTTP: &http.Client{Transport: g}} }

func (g *fakeGitHub) requests() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.seen...)
}

// fakeClock is a clock a test moves: each After is asked on waits, and
// fires when the test calls next.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	waits chan wait
}

type wait struct {
	d  time.Duration
	ch chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC), waits: make(chan wait, 16)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.waits <- wait{d: d, ch: ch}
	return ch
}

// next waits for the flow's next wait, checks it is want, moves the clock
// past it and fires it.
func (c *fakeClock) next(t *testing.T, want time.Duration) {
	t.Helper()
	select {
	case w := <-c.waits:
		if w.d != want {
			t.Fatalf("the flow waits %s, want %s", w.d, want)
		}
		c.mu.Lock()
		c.now = c.now.Add(w.d)
		now := c.now
		c.mu.Unlock()
		w.ch <- now
	case <-time.After(10 * time.Second):
		t.Fatal("the flow never waited")
	}
}

// keeper records what Keep got, and answers err.
type keeper struct {
	mu       sync.Mutex
	err      error
	kept     []string
	accounts []string
	hold     chan struct{}
	entered  chan struct{}
}

func (k *keeper) keep(_ context.Context, provider, token, account string) error {
	if k.entered != nil {
		close(k.entered)
	}
	if k.hold != nil {
		<-k.hold
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.kept = append(k.kept, provider+" "+token)
	k.accounts = append(k.accounts, account)
	return k.err
}

// changes records each Changed, and lets a test wait for the next one.
type changes struct{ ch chan string }

func newChanges() *changes { return &changes{ch: make(chan string, 64)} }

func (c *changes) changed(provider string) { c.ch <- provider }

func (c *changes) next(t *testing.T) {
	t.Helper()
	select {
	case <-c.ch:
	case <-time.After(10 * time.Second):
		t.Fatal("no change came")
	}
}
