package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/signins"
)

// The secrets a device flow handles, which no answer or event may carry.
const (
	deviceSecret = "dev-code-secret"
	tokenSecret  = "gho_granted_secret"
)

// deviceGitHub is GitHub's device flow in memory: the start's code, then
// one answer a poll, the last again once they run out, and the account.
type deviceGitHub struct {
	mu    sync.Mutex
	code  string
	polls []string
}

func (g *deviceGitHub) RoundTrip(r *http.Request) (*http.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	body := ""
	switch r.URL.String() {
	case "https://github.com/login/device/code":
		body = g.code
	case "https://github.com/login/oauth/access_token":
		body = g.polls[0]
		if len(g.polls) > 1 {
			g.polls = g.polls[1:]
		}
	case "https://api.github.com/user":
		body = `{"login":"octocat"}`
	default:
		return nil, errors.New("no such GitHub endpoint: " + r.URL.String())
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r}, nil
}

func (g *deviceGitHub) answer(polls ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.polls = polls
}

// fakeCredentials is the engine's credential store in memory: provider's
// stored sign-in and the token it was kept from.
type fakeCredentials struct {
	mu      sync.Mutex
	stored  map[string]signins.Stored
	tokens  map[string]string
	outs    int
	readErr error
}

func (c *fakeCredentials) StoredSignIn(_ context.Context, provider string) (signins.Stored, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stored[provider], c.readErr
}

func (c *fakeCredentials) KeepSignIn(_ context.Context, provider, token string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokens[provider] = token
	c.stored[provider] = signins.Stored{Kind: signins.KindOAuth, Refreshable: true, Identity: "id-" + provider}
	return "id-" + provider, nil
}

func (c *fakeCredentials) SignOut(_ context.Context, provider string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.outs++
	delete(c.stored, provider)
	delete(c.tokens, provider)
	return nil
}

// signInClock is the flows' clock: each wait is asked on waits, and fires
// when the test calls next.
type signInClock struct {
	waits chan signInWait
	now   *clock
}

type signInWait struct {
	d  time.Duration
	ch chan time.Time
}

func (c *signInClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.waits <- signInWait{d: d, ch: ch}
	return ch
}

func (c *signInClock) next(t *testing.T) {
	t.Helper()
	select {
	case w := <-c.waits:
		c.now.Advance(w.d)
		w.ch <- c.now.Now()
	case <-time.After(10 * time.Second):
		t.Fatal("the device flow never waited")
	}
}

// recorder keeps every answer's body a client got.
type recorder struct {
	next   http.RoundTripper
	mu     sync.Mutex
	bodies []string
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.next.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	r.mu.Lock()
	r.bodies = append(r.bodies, string(data))
	r.mu.Unlock()
	resp.Body = io.NopCloser(bytes.NewReader(data))
	return resp, nil
}

type signInHarness struct {
	*harness
	gh     *deviceGitHub
	creds  *fakeCredentials
	clock  *signInClock
	client *proto.Client
	rec    *recorder
	mu     sync.Mutex
	events []proto.Event
	evCh   chan proto.Event
}

const deviceCode = `{"device_code":"` + deviceSecret + `","user_code":"WDJB-MJHT","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`

func newSignInHarness(t *testing.T) *signInHarness {
	t.Helper()
	h := newHarness(t)
	sh := &signInHarness{harness: h, gh: &deviceGitHub{code: deviceCode, polls: []string{`{"error":"authorization_pending"}`}},
		creds: &fakeCredentials{stored: map[string]signins.Stored{}, tokens: map[string]string{}},
		clock: &signInClock{waits: make(chan signInWait, 16), now: h.clock}, evCh: make(chan proto.Event, 64)}
	emit := h.server.emit
	h.server.emit = func(e proto.Event) {
		if e.Type == proto.EventSignIn {
			sh.mu.Lock()
			sh.events = append(sh.events, e)
			sh.mu.Unlock()
			sh.evCh <- e
		}
		emit(e)
	}
	h.server.SetSignIns(sh.creds, signins.Options{GitHub: &signins.GitHub{HTTP: &http.Client{Transport: sh.gh}}, After: sh.clock.After})
	sh.rec = &recorder{next: h.http.Transport}
	sh.client, _ = proto.NewClient(base, h.token, &http.Client{Transport: sh.rec})
	return sh
}

// event waits for the next signin event.
func (sh *signInHarness) event(t *testing.T) proto.SignIn {
	t.Helper()
	select {
	case e := <-sh.evCh:
		return *e.SignIn
	case <-time.After(10 * time.Second):
		t.Fatal("no signin event came")
	}
	return proto.SignIn{}
}

// noSecrets fails when an answer or an event held the device code or the
// token.
func (sh *signInHarness) noSecrets(t *testing.T) {
	t.Helper()
	sh.rec.mu.Lock()
	all := strings.Join(sh.rec.bodies, "\n")
	sh.rec.mu.Unlock()
	sh.mu.Lock()
	for _, e := range sh.events {
		data, _ := json.Marshal(e)
		all += "\n" + string(data)
	}
	sh.mu.Unlock()
	for _, secret := range []string{deviceSecret, tokenSecret} {
		if strings.Contains(all, secret) {
			t.Fatalf("a client got %s: %s", secret, all)
		}
	}
}

// The owner signs pocketd in to GitHub Copilot from a client: the start
// answers the code, a retry and a second start answer the same flow,
// pocketd polls GitHub at the interval, keeps the approved sign-in through
// the engine's credential store with GitHub's login, and publishes each
// change; a sign-out takes it out again. No answer and no event carries
// the token or the device code (rule 69).
func TestTheOwnerSignsPocketdInToCopilotFromAClient(t *testing.T) {
	sh := newSignInHarness(t)
	ctx := context.Background()
	list, err := sh.client.SignIns(ctx)
	if err != nil || len(list) != len(signins.Providers) {
		t.Fatalf("SignIns %+v %v", list, err)
	}
	if c := list[0]; c.Provider != signins.Copilot || c.Name != "GitHub Copilot" || c.State != proto.SignInNone || !c.DeviceFlow || c.Flow != nil {
		t.Fatalf("Copilot before a sign-in %+v", c)
	}
	if list[1].DeviceFlow || list[1].State != proto.SignInNone {
		t.Fatalf("%+v", list[1])
	}

	// GitHub's answers are set before the flow polls: the first poll
	// waits, the second gets the token.
	sh.gh.answer(`{"error":"authorization_pending"}`, `{"access_token":"`+tokenSecret+`","token_type":"bearer"}`)
	flow, err := sh.client.StartSignIn(ctx, signins.Copilot, proto.StartSignIn{RequestID: "s1"})
	if err != nil || flow.State != proto.FlowWaiting || flow.UserCode != "WDJB-MJHT" || flow.VerificationURI != signins.DevicePage || flow.Provider != signins.Copilot {
		t.Fatalf("StartSignIn %+v %v", flow, err)
	}
	if row := sh.event(t); row.Flow == nil || row.Flow.ID != flow.ID || row.State != proto.SignInNone {
		t.Fatalf("the start's event %+v", row)
	}
	response, _ := sh.raw("POST", proto.Path(proto.PathSignIn, signins.Copilot), `{"requestId":"s1"}`, sh.headers())
	if response.StatusCode != http.StatusOK {
		t.Fatalf("the retry answered %d", response.StatusCode)
	}
	again, err := sh.client.StartSignIn(ctx, signins.Copilot, proto.StartSignIn{RequestID: "s2"})
	if err != nil || again.ID != flow.ID {
		t.Fatalf("a second start %+v %v", again, err)
	}
	if read, err := sh.client.SignInFlow(ctx, flow.ID); err != nil || read.ID != flow.ID || read.State != proto.FlowWaiting {
		t.Fatalf("SignInFlow %+v %v", read, err)
	}

	sh.clock.next(t)
	sh.clock.next(t)
	row := sh.event(t)
	if row.State != proto.SignInSignedIn || row.Account != "octocat" || row.Flow == nil || row.Flow.State != proto.FlowApproved || row.Flow.Account != "octocat" {
		t.Fatalf("the approval's event %+v %+v", row, row.Flow)
	}
	sh.creds.mu.Lock()
	kept := sh.creds.tokens[signins.Copilot]
	sh.creds.mu.Unlock()
	if kept != tokenSecret {
		t.Fatalf("the store kept %q", kept)
	}
	list, _ = sh.client.SignIns(ctx)
	if list[0].State != proto.SignInSignedIn || list[0].Account != "octocat" {
		t.Fatalf("Copilot after the sign-in %+v", list[0])
	}
	// The account stays beside its credential alone: a sign-in made since
	// another way shows none.
	sh.creds.mu.Lock()
	sh.creds.stored[signins.Copilot] = signins.Stored{Kind: signins.KindOAuth, Refreshable: true, Identity: "another"}
	sh.creds.mu.Unlock()
	if list, _ = sh.client.SignIns(ctx); list[0].Account != "" {
		t.Fatalf("another credential shows the account %q", list[0].Account)
	}

	out, err := sh.client.SignOut(ctx, signins.Copilot)
	if err != nil || out.State != proto.SignInNone || out.Account != "" || sh.creds.outs != 1 {
		t.Fatalf("SignOut %+v %v (%d)", out, err, sh.creds.outs)
	}
	if row := sh.event(t); row.State != proto.SignInNone {
		t.Fatalf("the sign-out's event %+v", row)
	}
	if _, _, ok, _ := sh.store.SignInAccount(ctx, signins.Copilot); ok {
		t.Fatal("the sign-out kept the account")
	}
	sh.noSecrets(t)
}

func (sh *signInHarness) headers() map[string]string {
	return map[string]string{proto.HeaderProtocol: strconv.Itoa(proto.Version), "Authorization": "Bearer " + sh.token, "Content-Type": "application/json"}
}

// A canceled flow ends canceled and polls no more, a retry of its cancel
// answers it again, and a sign-out cancels a waiting flow; a denied code
// ends denied, and a new start after an end is a new flow.
func TestCancelDenialAndSignOutEndTheFlow(t *testing.T) {
	sh := newSignInHarness(t)
	ctx := context.Background()
	flow, _ := sh.client.StartSignIn(ctx, signins.Copilot, proto.StartSignIn{RequestID: "s1"})
	sh.event(t)
	canceled, err := sh.client.CancelSignIn(ctx, flow.ID, proto.CancelSignIn{RequestID: "c1"})
	if err != nil || canceled.State != proto.FlowCanceled {
		t.Fatalf("CancelSignIn %+v %v", canceled, err)
	}
	if row := sh.event(t); row.Flow.State != proto.FlowCanceled {
		t.Fatalf("the cancel's event %+v", row.Flow)
	}
	if again, err := sh.client.CancelSignIn(ctx, flow.ID, proto.CancelSignIn{RequestID: "c1"}); err != nil || again.State != proto.FlowCanceled {
		t.Fatalf("a retried cancel %+v %v", again, err)
	}

	second, err := sh.client.StartSignIn(ctx, signins.Copilot, proto.StartSignIn{RequestID: "s2"})
	if err != nil || second.ID == flow.ID || second.State != proto.FlowWaiting {
		t.Fatalf("a start after the cancel %+v %v", second, err)
	}
	sh.event(t)
	if _, err := sh.client.SignOut(ctx, signins.Copilot); err != nil {
		t.Fatal(err)
	}
	if row := sh.event(t); row.Flow.ID != second.ID || row.Flow.State != proto.FlowCanceled {
		t.Fatalf("the sign-out left the flow %+v", row.Flow)
	}
	sh.event(t)

	third, _ := sh.client.StartSignIn(ctx, signins.Copilot, proto.StartSignIn{RequestID: "s3"})
	sh.event(t)
	sh.gh.answer(`{"error":"access_denied"}`)
	if row := sh.fireUntilEvent(t); row.Flow.ID != third.ID || row.Flow.State != proto.FlowDenied || row.State != proto.SignInNone {
		t.Fatalf("the denial's event %+v", row.Flow)
	}
	sh.noSecrets(t)
}

// fireUntilEvent fires each wait the flows ask, a canceled flow's left
// over among them, until the next signin event comes.
func (sh *signInHarness) fireUntilEvent(t *testing.T) proto.SignIn {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case w := <-sh.clock.waits:
			sh.clock.now.Advance(w.d)
			w.ch <- sh.clock.now.Now()
		case e := <-sh.evCh:
			return *e.SignIn
		case <-deadline:
			t.Fatal("no signin event came")
		}
	}
}

// What a client may not do: start or sign out of a provider that signs in
// on the service host, or of none; read or cancel a flow pocketd does not
// keep; reuse a request id for another request; retry a start whose flow
// a restart lost.
func TestSignInsRefuse(t *testing.T) {
	sh := newSignInHarness(t)
	ctx := context.Background()
	_, err := sh.client.StartSignIn(ctx, "anthropic", proto.StartSignIn{RequestID: "a1"})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	if !strings.Contains(err.Error(), "pocketd login anthropic") {
		t.Fatalf("the refusal %v", err)
	}
	_, err = sh.client.StartSignIn(ctx, "nobody", proto.StartSignIn{RequestID: "a2"})
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
	_, err = sh.client.SignOut(ctx, "openai-codex")
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)
	_, err = sh.client.SignInFlow(ctx, "nope")
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
	_, err = sh.client.CancelSignIn(ctx, "nope", proto.CancelSignIn{RequestID: "a3"})
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)
	_, err = sh.client.StartSignIn(ctx, signins.Copilot, proto.StartSignIn{})
	asError(t, err, http.StatusBadRequest, proto.ErrorCodeBadRequest)

	flow, _ := sh.client.StartSignIn(ctx, signins.Copilot, proto.StartSignIn{RequestID: "s1"})
	_, err = sh.client.CancelSignIn(ctx, flow.ID, proto.CancelSignIn{RequestID: "s1"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if _, err := sh.client.CancelSignIn(ctx, flow.ID, proto.CancelSignIn{RequestID: "c1"}); err != nil {
		t.Fatal(err)
	}
	_, err = sh.client.CancelSignIn(ctx, "another", proto.CancelSignIn{RequestID: "c1"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)

	// A start recorded by an earlier run, whose flow is gone with it.
	if err := sh.store.RecordRequest(ctx, requestKeyOf(sh.harness, "old"), startSignInKind+signins.Copilot, "gone-flow"); err != nil {
		t.Fatal(err)
	}
	_, err = sh.client.StartSignIn(ctx, signins.Copilot, proto.StartSignIn{RequestID: "old"})
	asError(t, err, http.StatusNotFound, proto.ErrorCodeNotFound)

	response, _ := sh.raw("GET", proto.Path(proto.PathSignIn, signins.Copilot), "", sh.headers())
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET of a provider answered %d", response.StatusCode)
	}
}

// A signin event whose store read fails goes out with the state last
// published, never none in place of a sign-in.
func TestASignInEventKeepsItsLastStateWhenTheStoreFails(t *testing.T) {
	sh := newSignInHarness(t)
	sh.creds.stored[signins.Copilot] = signins.Stored{Kind: signins.KindOAuth, Refreshable: true}
	sh.server.publishSignIn(signins.Copilot)
	if row := sh.event(t); row.State != proto.SignInSignedIn {
		t.Fatalf("%+v", row)
	}
	sh.creds.mu.Lock()
	sh.creds.readErr = errors.New("auth.json unreadable")
	sh.creds.mu.Unlock()
	sh.server.publishSignIn(signins.Copilot)
	if row := sh.event(t); row.State != proto.SignInSignedIn {
		t.Fatalf("a failed read published %+v", row)
	}
	_, err := sh.client.SignIns(context.Background())
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
}
