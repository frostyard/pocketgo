package signins

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// flowsOn are flows on a fake GitHub and a fake clock, stopped at the
// test's end.
func flowsOn(t *testing.T, g *fakeGitHub, k *keeper) (*Flows, *fakeClock, *changes) {
	t.Helper()
	clock, ch := newFakeClock(), newChanges()
	f := New(Options{GitHub: g.client(), Keep: k.keep, Changed: ch.changed, Now: clock.Now, After: clock.After})
	t.Cleanup(f.Stop)
	return f, clock, ch
}

// waitEnded waits for flow id to end, by its Changed.
func waitEnded(t *testing.T, f *Flows, ch *changes, id string) proto.SignInFlow {
	t.Helper()
	for range 10 {
		if flow, _ := f.Flow(id); flow.State.Ended() {
			return flow
		}
		ch.next(t)
	}
	t.Fatal("the flow never ended")
	return proto.SignInFlow{}
}

// noSecret fails when what a client gets holds the token or the device
// code.
func noSecret(t *testing.T, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"gho_granted_secret", "dev-code-secret"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("%s holds a secret", data)
		}
	}
}

// A flow polls after each interval, keeps going while the code waits, and
// once approved keeps the sign-in with the account GitHub names: approved,
// with no token or device code in anything a client gets (rule 69).
func TestAnApprovedFlowKeepsTheSignIn(t *testing.T) {
	g := newFakeGitHub(t, ok(pending), ok(granted))
	k := &keeper{}
	f, clock, ch := flowsOn(t, g, k)
	flow, started, err := f.Start(Copilot, "k1")
	if err != nil || !started {
		t.Fatalf("Start %+v %v %v", flow, started, err)
	}
	ch.next(t)
	if flow.State != proto.FlowWaiting || flow.UserCode != "WDJB-MJHT" || flow.VerificationURI != DevicePage ||
		flow.ExpiresIn != 900 || flow.Interval != 5 || !flow.ExpiresAt.Equal(flow.StartedAt.Add(900*time.Second)) {
		t.Fatalf("the started flow %+v", flow)
	}
	noSecret(t, flow)
	clock.next(t, 5*time.Second)
	clock.next(t, 5*time.Second)
	flow = waitEnded(t, f, ch, flow.ID)
	if flow.State != proto.FlowApproved || flow.Account != "octocat" || flow.Error != "" || flow.EndedAt.IsZero() {
		t.Fatalf("the approved flow %+v", flow)
	}
	noSecret(t, flow)
	if len(k.kept) != 1 || k.kept[0] != Copilot+" gho_granted_secret" || k.accounts[0] != "octocat" {
		t.Fatalf("kept %v %v", k.kept, k.accounts)
	}
	want := []string{"POST https://github.com/login/device/code", "POST https://github.com/login/oauth/access_token",
		"POST https://github.com/login/oauth/access_token", "GET https://api.github.com/user"}
	if got := g.requests(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests %v", got)
	}
	if g.forms[0].Get("client_id") != copilotClientID || g.forms[0].Get("scope") != "read:user" || g.forms[1].Get("device_code") != "dev-code-secret" ||
		g.forms[1].Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || g.auths[3] != "Bearer gho_granted_secret" || g.auths[1] != "" {
		t.Fatalf("forms %v, authorizations %v", g.forms, g.auths)
	}
}

// slow_down widens the wait by 5 seconds, or to the interval GitHub names
// when longer (RFC 8628, 3.5), for every later poll; a denied code ends
// the flow denied.
func TestSlowDownWidensTheWaitAndADenialEndsTheFlow(t *testing.T) {
	g := newFakeGitHub(t, ok(slowDown), ok(`{"error":"slow_down"}`), ok(pending), ok(denied))
	f, clock, ch := flowsOn(t, g, &keeper{})
	flow, _, _ := f.Start(Copilot, "k2")
	ch.next(t)
	clock.next(t, 5*time.Second)
	clock.next(t, 10*time.Second)
	clock.next(t, 15*time.Second)
	if now, _ := f.Flow(flow.ID); now.Interval != 15 {
		t.Fatalf("the interval after two slow_downs %d", now.Interval)
	}
	clock.next(t, 15*time.Second)
	flow = waitEnded(t, f, ch, flow.ID)
	if flow.State != proto.FlowDenied || !strings.Contains(flow.Error, "denied") {
		t.Fatalf("the denied flow %+v", flow)
	}
	if SlowedDown(5*time.Second, 2*time.Second) != 10*time.Second || SlowedDown(maxInterval, 0) != maxInterval+slowDownStep {
		t.Fatal("SlowedDown")
	}
}

// A code polls no later than its expiry, and ends expired then, as it does
// when GitHub says it expired.
func TestACodeExpires(t *testing.T) {
	g := newFakeGitHub(t, ok(pending))
	g.code = ok(`{"device_code":"dev-code-secret","user_code":"WDJB-MJHT","verification_uri":"https://github.com/login/device","expires_in":12,"interval":5}`)
	f, clock, ch := flowsOn(t, g, &keeper{})
	flow, _, _ := f.Start(Copilot, "k3")
	ch.next(t)
	clock.next(t, 5*time.Second)
	clock.next(t, 5*time.Second)
	clock.next(t, 2*time.Second)
	flow = waitEnded(t, f, ch, flow.ID)
	if flow.State != proto.FlowExpired || len(g.requests()) != 3 {
		t.Fatalf("the expired flow %+v after %v", flow, g.requests())
	}

	g2 := newFakeGitHub(t, ok(expired))
	f2, clock2, ch2 := flowsOn(t, g2, &keeper{})
	flow, _, _ = f2.Start(Copilot, "k4")
	ch2.next(t)
	clock2.next(t, 5*time.Second)
	if flow = waitEnded(t, f2, ch2, flow.ID); flow.State != proto.FlowExpired {
		t.Fatalf("GitHub's expired_token: %+v", flow)
	}
}

// A canceled flow ends at once and polls no more; a cancel of an ended
// flow changes nothing.
func TestACanceledFlowPollsNoMore(t *testing.T) {
	g := newFakeGitHub(t, ok(pending))
	f, _, ch := flowsOn(t, g, &keeper{})
	flow, _, _ := f.Start(Copilot, "k5")
	ch.next(t)
	canceled, ok := f.Cancel(flow.ID)
	if !ok || canceled.State != proto.FlowCanceled || canceled.EndedAt.IsZero() {
		t.Fatalf("Cancel %+v %v", canceled, ok)
	}
	ch.next(t)
	again, _ := f.Cancel(flow.ID)
	if again != canceled {
		t.Fatalf("a second cancel changed the flow: %+v", again)
	}
	f.Stop()
	if got := g.requests(); len(got) != 1 {
		t.Fatalf("a canceled flow polled: %v", got)
	}
	if _, ok := f.Cancel("nope"); ok {
		t.Fatal("an unknown flow was canceled")
	}
}

// One flow a provider: a second start answers the running one; once it
// ended, a start makes a new one, and the old one is no longer kept. Only
// a device flow's provider starts one.
func TestOneFlowAProviderAtATime(t *testing.T) {
	g := newFakeGitHub(t, ok(pending))
	f, _, ch := flowsOn(t, g, &keeper{})
	first, _, _ := f.Start(Copilot, "k6")
	ch.next(t)
	second, again, err := f.Start(Copilot, "k7")
	if err != nil || again || second.ID != first.ID {
		t.Fatalf("a second start %+v %v %v", second, again, err)
	}
	f.Cancel(first.ID)
	ch.next(t)
	third, started, _ := f.Start(Copilot, "k8")
	ch.next(t)
	if !started || third.ID == first.ID {
		t.Fatalf("a start after the end %+v %v", third, started)
	}
	if _, ok := f.Flow(first.ID); ok {
		t.Fatal("the replaced flow is still kept")
	}
	if latest, _ := f.Latest(Copilot); latest.ID != third.ID {
		t.Fatalf("the latest flow %+v", latest)
	}
	if _, _, err := f.Start("anthropic", "k9"); !errors.Is(err, ErrNoDeviceFlow) {
		t.Fatalf("anthropic started a device flow: %v", err)
	}
}

// A start GitHub did not answer with a code is a failed flow, kept and
// answered like any other; polls that fail to reach GitHub are tried again
// up to 3 times in a row, a good answer between them starting the count
// again.
func TestNetworkFailures(t *testing.T) {
	g := newFakeGitHub(t)
	g.code = answer{status: 503, body: "down"}
	f, _, ch := flowsOn(t, g, &keeper{})
	flow, started, err := f.Start(Copilot, "k10")
	if err != nil || !started || flow.State != proto.FlowFailed || !strings.Contains(flow.Error, "503") || flow.UserCode != "" {
		t.Fatalf("a start GitHub failed %+v %v %v", flow, started, err)
	}
	ch.next(t)
	if kept, ok := f.Flow(flow.ID); !ok || kept != flow {
		t.Fatalf("the failed flow is not kept: %+v", kept)
	}

	down := answer{err: errors.New("connection refused")}
	g2 := newFakeGitHub(t, down, answer{status: 502, body: "bad gateway"}, ok(pending), down, answer{status: 429}, down)
	f2, clock, ch2 := flowsOn(t, g2, &keeper{})
	flow, _, _ = f2.Start(Copilot, "k11")
	ch2.next(t)
	for range 5 {
		clock.next(t, 5*time.Second)
	}
	if now, _ := f2.Flow(flow.ID); now.State != proto.FlowWaiting {
		t.Fatalf("two failures in a row ended the flow: %+v", now)
	}
	clock.next(t, 5*time.Second)
	flow = waitEnded(t, f2, ch2, flow.ID)
	if flow.State != proto.FlowFailed || !strings.Contains(flow.Error, "could not be reached") {
		t.Fatalf("three failures in a row: %+v", flow)
	}
}

// A sign-in Keep could not keep fails the flow, with an error that does
// not hold the token; an account GitHub does not name is kept as none.
func TestAKeepThatFailsSaysWhyWithoutTheToken(t *testing.T) {
	g := newFakeGitHub(t, ok(granted))
	g.user = answer{status: 401, body: `{"message":"Bad credentials"}`}
	k := &keeper{err: errors.New("Copilot refused gho_granted_secret")}
	f, clock, ch := flowsOn(t, g, k)
	flow, _, _ := f.Start(Copilot, "k12")
	ch.next(t)
	clock.next(t, 5*time.Second)
	flow = waitEnded(t, f, ch, flow.ID)
	if flow.State != proto.FlowFailed || !strings.Contains(flow.Error, "could not keep") {
		t.Fatalf("the flow %+v", flow)
	}
	noSecret(t, flow)
	if len(k.accounts) != 1 || k.accounts[0] != "" {
		t.Fatalf("accounts %v", k.accounts)
	}
}

// A sign-out cancels the waiting flow, so nothing it gets is kept; one
// that comes while an approved flow's sign-in is kept waits for it, and
// removes what it kept; a cancel then changes nothing.
func TestASignOutAndAKeepGoInTurn(t *testing.T) {
	g := newFakeGitHub(t, ok(pending))
	f, _, ch := flowsOn(t, g, &keeper{})
	flow, _, _ := f.Start(Copilot, "k13")
	ch.next(t)
	removed := 0
	if err := f.SignOut(context.Background(), Copilot, func(context.Context) error { removed++; return nil }); err != nil || removed != 1 {
		t.Fatalf("SignOut %v, removed %d", err, removed)
	}
	if now, _ := f.Flow(flow.ID); now.State != proto.FlowCanceled {
		t.Fatalf("the sign-out left the flow %+v", now)
	}
	if err := f.SignOut(context.Background(), "anthropic", func(context.Context) error { return nil }); !errors.Is(err, ErrNoDeviceFlow) {
		t.Fatalf("anthropic signed out: %v", err)
	}

	g2 := newFakeGitHub(t, ok(granted))
	k := &keeper{hold: make(chan struct{}), entered: make(chan struct{})}
	f2, clock, ch2 := flowsOn(t, g2, k)
	flow, _, _ = f2.Start(Copilot, "k14")
	ch2.next(t)
	clock.next(t, 5*time.Second)
	<-k.entered
	if now, _ := f2.Cancel(flow.ID); now.State != proto.FlowWaiting {
		t.Fatalf("a cancel took a flow whose sign-in is kept: %+v", now)
	}
	signedOut := make(chan []string, 1)
	go func() {
		_ = f2.SignOut(context.Background(), Copilot, func(context.Context) error {
			k.mu.Lock()
			defer k.mu.Unlock()
			signedOut <- append([]string(nil), k.kept...)
			return nil
		})
	}()
	close(k.hold)
	if kept := <-signedOut; len(kept) != 1 {
		t.Fatalf("the sign-out ran before the keep ended: %v", kept)
	}
	if flow = waitEnded(t, f2, ch2, flow.ID); flow.State != proto.FlowApproved {
		t.Fatalf("the kept flow %+v", flow)
	}
}

// After Stop no flow starts, and a running one ends.
func TestStopEndsTheFlows(t *testing.T) {
	g := newFakeGitHub(t, ok(pending))
	f, _, ch := flowsOn(t, g, &keeper{})
	flow, _, _ := f.Start(Copilot, "k15")
	ch.next(t)
	f.Stop()
	if now, _ := f.Flow(flow.ID); now.State != proto.FlowCanceled || now.Error != "pocketd stopped." {
		t.Fatalf("a flow after Stop %+v", now)
	}
	if _, _, err := f.Start(Copilot, "k16"); !errors.Is(err, ErrStopped) {
		t.Fatalf("a start after Stop: %v", err)
	}
}

// blockingStart is a GitHub whose device code request waits for its
// context to end, says so on canceled, and then waits for release; any
// later request fails the test.
type blockingStart struct {
	t                          *testing.T
	calls                      atomic.Int32
	entered, canceled, release chan struct{}
}

func (b *blockingStart) RoundTrip(r *http.Request) (*http.Response, error) {
	if b.calls.Add(1) != 1 || r.URL.String() != "https://github.com/login/device/code" {
		b.t.Errorf("a request to GitHub after the first start: %s %s", r.Method, r.URL)
		return nil, errors.New("unexpected")
	}
	close(b.entered)
	<-r.Context().Done()
	close(b.canceled)
	<-b.release
	return nil, r.Context().Err()
}

// stopWaits says whether a goroutine is parked in Flows.Stop itself:
// blocked (not running, runnable or in a system call), with Stop as its
// first frame outside the runtime and sync packages. A Stop that does not
// wait for a start in progress never parks there, since nothing holds what
// else it takes; so the test tells "Stop waits" from "Stop is slow" with
// no timer.
func stopWaits() bool {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		head, frames, _ := strings.Cut(g, "\n")
		_, state, _ := strings.Cut(head, "[")
		state, _, _ = strings.Cut(state, "]")
		state, _, _ = strings.Cut(state, ",")
		if state == "running" || state == "runnable" || strings.HasPrefix(state, "syscall") {
			continue
		}
		for _, line := range strings.Split(frames, "\n") {
			if line == "" || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "runtime.") ||
				strings.HasPrefix(line, "sync.") || strings.HasPrefix(line, "internal/") {
				continue
			}
			if strings.Contains(line, "signins.(*Flows).Stop(") {
				return true
			}
			break
		}
	}
	return false
}

// Stop waits for a start in progress: its cancel ends the start's request
// to GitHub, and Stop returns only once that start returned, with
// ErrStopped and no flow kept. A start after Stop asks GitHub nothing
// (#130).
func TestStopWaitsForAStartInProgress(t *testing.T) {
	b := &blockingStart{t: t, entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	clock, ch := newFakeClock(), newChanges()
	f := New(Options{GitHub: &GitHub{HTTP: &http.Client{Transport: b}}, Keep: (&keeper{}).keep, Changed: ch.changed, Now: clock.Now, After: clock.After})
	defer func() {
		select {
		case <-b.release:
		default:
			close(b.release)
		}
	}()
	started := make(chan error, 1)
	go func() {
		_, _, err := f.Start(Copilot, "k17")
		started <- err
	}()
	<-b.entered
	stopped := make(chan struct{})
	go func() {
		f.Stop()
		close(stopped)
	}()
	<-b.canceled
	for !stopWaits() {
		select {
		case <-stopped:
			t.Fatal("Stop returned while a start was in progress")
		default:
			runtime.Gosched()
		}
	}
	select {
	case <-stopped:
		t.Fatal("Stop returned while a start was in progress")
	default:
	}
	close(b.release)
	if err := <-started; !errors.Is(err, ErrStopped) {
		t.Fatalf("the start in progress at Stop: %v", err)
	}
	<-stopped
	if _, ok := f.Latest(Copilot); ok {
		t.Fatal("a start in progress at Stop kept a flow")
	}
	if _, _, err := f.Start(Copilot, "k18"); !errors.Is(err, ErrStopped) {
		t.Fatalf("a start after Stop: %v", err)
	}
	if n := b.calls.Load(); n != 1 {
		t.Fatalf("GitHub got %d requests, want 1", n)
	}
	select {
	case p := <-ch.ch:
		t.Fatalf("a change of %s after Stop", p)
	default:
	}
}
