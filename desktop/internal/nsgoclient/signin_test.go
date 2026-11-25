package nsgoclient

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/nsgotest"
)

// testKeyring keeps device tokens as a keyring would, in memory, with the
// failures a test sets: no test touches a real keyring. Its lock is for
// the credential jobs, which run on goroutines of their own.
type testKeyring struct {
	mu                sync.Mutex
	tokens            map[string]string
	setErr, forgetErr error
}

func newTestKeyring() *testKeyring { return &testKeyring{tokens: map[string]string{}} }

func (k *testKeyring) Token(server string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.tokens[server], nil
}

func (k *testKeyring) SetToken(server, token string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.setErr != nil {
		return k.setErr
	}
	k.tokens[server] = token
	return nil
}

func (k *testKeyring) Forget(server string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.forgetErr != nil {
		return k.forgetErr
	}
	delete(k.tokens, server)
	return nil
}

func (k *testKeyring) kept(server string) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.tokens[server]
}

func (k *testKeyring) failForget(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.forgetErr = err
}

// signInScreen starts the client on the sign-in screen with ring as the
// device's keyring.
func signInScreen(t *testing.T, transport http.RoundTripper, ring *testKeyring, opts ...nsgotest.Option) *nsgotest.App {
	t.Helper()
	app := nsgotest.Start(t, func(cx *core.Scope) core.Node {
		cx.SetAppearance(core.AppearanceLight)
		return startClient(cx, transport, false, func(c *client) { c.creds = ring })
	}, append(opts, nsgotest.Size(1230, 800))...)
	app.WaitFor("label:Device token", 5*time.Second)
	return app
}

// typeSignIn fills the form with server and token.
func typeSignIn(app *nsgotest.App, server, token string) {
	app.Clear("label:Service URL")
	app.Type("label:Service URL", server)
	app.Clear("label:Device token")
	app.Type("label:Device token", token)
}

// problemShown is what the sign-in screen's banner says, "" with none.
func problemShown(app *nsgotest.App) string {
	if !app.Exists("id:banner") {
		return ""
	}
	return app.Find("id:banner").Props.Label
}

// TestTheSignInScreenHasTheMyGoClientsTexts checks the screen's texts and
// fields: what to enter and where the token is kept, a placeholder that
// reads as an example, a masked token, the keyboard in the empty address,
// and a refused token's reason on the screen. TestSignInSnapshot holds
// the same for the MyGo client.
func TestTheSignInScreenHasTheMyGoClientsTexts(t *testing.T) {
	s := pocketdtest.New()
	ring := newTestKeyring()
	app := signInScreen(t, s.Transport(), ring)
	app.Find("text:Connect to pocketd")
	app.Find("text:paste only the pgd_… value it prints. PocketGo keeps the token in this device's keyring.")
	address, token := app.Find("label:Service URL"), app.Find("label:Device token")
	// The owner once took a placeholder for a value filled in.
	if !strings.HasPrefix(address.Input.Placeholder, "e.g. ") {
		t.Errorf("the address's placeholder %q reads like a value", address.Input.Placeholder)
	}
	if !token.Input.Masked || token.Input.Placeholder != "pgd_…" {
		t.Errorf("the token field: masked %v, placeholder %q", token.Input.Masked, token.Input.Placeholder)
	}
	if !address.Focused {
		t.Error("the keyboard does not start in the empty address")
	}
	app.Screenshot("signin")

	typeSignIn(app, pocketdtest.URL, "pgd_wrong")
	app.Press("role:button Connect")
	app.WaitUntil(5*time.Second, func() bool { return problemShown(app) != "" })
	if got := problemShown(app); !strings.HasPrefix(got, "pocketd refused the device token") || !strings.Contains(got, "only the pgd_… value") {
		t.Errorf("a refused token says %q", got)
	}
	app.Clear("label:Device token")
	app.Type("label:Device token", pocketdtest.Token)
	app.Press("role:button Connect")
	app.WaitFor("label:"+paletteLabel, 5*time.Second)
	app.WaitUntil(5*time.Second, func() bool { return ring.kept(pocketdtest.URL) == pocketdtest.Token })
}

// TestABrowserSaysItKeepsNoToken checks the guide where the client has no
// store for the token, as in a browser (accounts_js.go): it says the
// owner signs in again. The MyGo client has no such build.
func TestABrowserSaysItKeepsNoToken(t *testing.T) {
	app := nsgotest.Start(t, func(cx *core.Scope) core.Node { return newClient(cx, pocketdtest.New().Transport()) }, nsgotest.Size(1230, 800))
	app.WaitFor("text:PocketGo does not keep the token here: you sign in again next time.", 5*time.Second)
	if app.Exists("text:keyring") {
		t.Error("a client with no store for the token names a keyring")
	}
}

// TestConnectSaysWhichFieldIsMissingOnNSGo checks that Connect with a
// field left empty, or an address that is not one, says so on the screen
// and sends nothing. TestConnectSaysWhichFieldIsMissing holds the same for
// the MyGo client.
func TestConnectSaysWhichFieldIsMissingOnNSGo(t *testing.T) {
	for _, tc := range []struct {
		name, server, token, want, picture string
	}{
		{"only the token", "", pocketdtest.Token, "Enter the service URL, such as http://pocketd.example.ts.net:8788", ""},
		{"only the URL", "http://pocket.example.ts.net:8788", "", "Paste the device token that pocketd device add printed.", "signin-missing-field"},
		{"no scheme", "pocketd.example.ts.net", pocketdtest.Token, "The service URL needs its scheme", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := pocketdtest.New()
			app := signInScreen(t, s.Transport(), newTestKeyring())
			typeSignIn(app, tc.server, tc.token)
			app.Press("role:button Connect")
			app.WaitUntil(5*time.Second, func() bool { return problemShown(app) != "" })
			if got := problemShown(app); !strings.HasPrefix(got, tc.want) {
				t.Errorf("the screen says %q, want it to start with %q", got, tc.want)
			}
			if tc.picture != "" {
				app.Screenshot(tc.picture)
			}
			if n := len(s.Requests()); n != 0 {
				t.Errorf("%d requests left with the form not filled", n)
			}
		})
	}
}

// TestTheKeyboardStartsInTheTokenAndReturnConnects checks a known
// address: the keyboard starts in the token, and Return in the address
// field connects as Connect does. The MyGo client's signInView holds the
// same (url.Submitted, token.AutoFocus); no MyGo test checks it.
func TestTheKeyboardStartsInTheTokenAndReturnConnects(t *testing.T) {
	s := pocketdtest.New()
	app := signInScreen(t, s.Transport(), newTestKeyring(), nsgotest.Storage(map[string]string{"last-server": pocketdtest.URL}))
	if got := app.Find("label:Service URL").Text; got != pocketdtest.URL {
		t.Errorf("the address shows %q, want the last service", got)
	}
	if !app.Find("label:Device token").Focused {
		t.Error("the keyboard does not start in the token")
	}
	app.Type("label:Device token", pocketdtest.Token)
	app.Submit("label:Service URL")
	app.WaitFor("label:"+paletteLabel, 5*time.Second)
}

// heldHello holds GET /v1/hello back until held closes.
type heldHello struct {
	base http.RoundTripper
	held chan struct{}
}

func (h heldHello) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == proto.PathHello {
		select {
		case <-h.held:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	return h.base.RoundTrip(r)
}

// TestASignInUnderWaySaysSo checks the screen while pocketd has not
// answered: it says it is signing in, and the fields take no input. The
// MyGo client's signInView holds the same (its spinner, "Signing in…");
// no MyGo test checks it.
func TestASignInUnderWaySaysSo(t *testing.T) {
	s := pocketdtest.New()
	held := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(held) }) }
	t.Cleanup(release)
	app := signInScreen(t, heldHello{s.Transport(), held}, newTestKeyring())
	typeSignIn(app, pocketdtest.URL, pocketdtest.Token)
	app.Press("role:button Connect")
	app.WaitFor("text:Signing in…", 5*time.Second)
	for _, field := range []string{"label:Service URL", "label:Device token"} {
		if app.Enabled(field) {
			t.Errorf("%s takes input while the sign-in runs", field)
		}
	}
	release()
	app.WaitFor("label:"+paletteLabel, 5*time.Second)
}

// tokenWatch records each request's host and whether it carried the
// device token, before the transport it wraps serves it.
type tokenWatch struct {
	base http.RoundTripper
	mu   sync.Mutex
	sent []string
}

func (w *tokenWatch) RoundTrip(r *http.Request) (*http.Response, error) {
	w.mu.Lock()
	if strings.Contains(r.Header.Get("Authorization"), pocketdtest.Token) {
		w.sent = append(w.sent, r.URL.Host)
	}
	w.mu.Unlock()
	return w.base.RoundTrip(r)
}

func (w *tokenWatch) withToken() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.sent...)
}

// TestNoTokenLeavesBeforeThePlainHTTPWord checks the question before the
// device token goes over plain HTTP to a host outside this computer and
// the tailnet: it names the address and says what the MyGo client's says,
// and no request leaves, with or without the token, until the owner's
// Connect anyway: not on Return, which answers Cancel, nor on Cancel.
// TestSignInSnapshot holds the same for the MyGo client.
func TestNoTokenLeavesBeforeThePlainHTTPWord(t *testing.T) {
	const server = "http://192.0.2.10:8080"
	s := pocketdtest.New()
	watch := &tokenWatch{base: pocketdtest.Hosts{"192.0.2.10:8080": s.Handler()}}
	app := signInScreen(t, watch, newTestKeyring())
	ask := func() {
		t.Helper()
		typeSignIn(app, server, pocketdtest.Token)
		app.Press("role:button Connect")
		app.WaitFor("role:alertdialog >> text:Connect over plain HTTP?", 5*time.Second)
	}
	nothingSent := func(when string) {
		t.Helper()
		if n := len(s.Requests()); n != 0 {
			t.Errorf("%s: %d requests left before the owner's word", when, n)
		}
		if hosts := watch.withToken(); len(hosts) != 0 {
			t.Errorf("%s: the token went to %v before the owner's word", when, hosts)
		}
	}
	ask()
	for _, said := range []string{
		"The device token will travel to " + server + " without HTTPS.",
		"its host is neither this computer nor a tailnet name (*.ts.net): the device token could travel in clear text. Use https://, or the service's MagicDNS name.",
	} {
		app.Find("role:alertdialog >> text:" + said)
	}
	app.Screenshot("signin-plain-http")
	nothingSent("asked")
	// Return answers Cancel where the keyboard is on it; where it stays in
	// a field (headless, nsgotest moves no focus to an alert's button), the
	// locked field asks nothing more.
	app.Key("Enter")
	nothingSent("after Return")
	if app.Exists("role:alertdialog") {
		app.Press("role:alertdialog >> role:button Cancel")
	}
	app.WaitForGone("role:alertdialog", 5*time.Second)
	ask()
	app.Press("role:alertdialog >> role:button Cancel")
	app.WaitForGone("role:alertdialog", 5*time.Second)
	nothingSent("after Cancel")
	ask()
	app.Press("role:alertdialog >> role:button Connect anyway")
	app.WaitFor("label:"+paletteLabel, 5*time.Second)
	hosts := watch.withToken()
	if len(hosts) == 0 || hosts[0] != "192.0.2.10:8080" {
		t.Errorf("after Connect anyway the token went to %v", hosts)
	}
}

// TestPocketdsSignOutSaysWhyOnTheSignInScreen checks a device pocketd no
// longer takes: the client signs out, forgets the token, and the sign-in
// screen it shows says why, with the service's address filled in.
// TestAKeptSignInConnectsAtStart holds the same for the MyGo client.
func TestPocketdsSignOutSaysWhyOnTheSignInScreen(t *testing.T) {
	s := pocketdtest.New()
	ring := newTestKeyring()
	app := signInScreen(t, s.Transport(), ring)
	typeSignIn(app, pocketdtest.URL, pocketdtest.Token)
	app.Press("role:button Connect")
	app.WaitFor("label:"+paletteLabel, 5*time.Second)
	app.WaitUntil(5*time.Second, func() bool { return ring.kept(pocketdtest.URL) != "" })
	s.Revoke()
	app.Press("role:toolbar >> label:Refresh")
	app.WaitFor("label:Device token", 5*time.Second)
	app.WaitUntil(5*time.Second, func() bool { return problemShown(app) != "" })
	if got := problemShown(app); !strings.HasPrefix(got, "pocketd no longer takes this device's token. Run `pocketd device add NAME`") {
		t.Errorf("the sign-in screen says %q", got)
	}
	if got := app.Find("label:Service URL").Text; got != pocketdtest.URL {
		t.Errorf("the address shows %q", got)
	}
	if app.Exists("role:alert") {
		t.Error("a toast lies over the screen that says why")
	}
	app.WaitUntil(5*time.Second, func() bool { return ring.kept(pocketdtest.URL) == "" })
	if kept, _ := app.Storage().GetItem("connected-server"); kept != "" {
		t.Errorf("the next start would sign in to %q", kept)
	}
	app.Screenshot("signin-signed-out")
}

// TestAKeyringThatKeepsTheTokenSaysSoOnTheSignInScreen checks a sign-out
// whose token the keyring will not forget: the sign-in screen says what
// was left and what to do, and the next start does not sign in with it.
// TestSigningOutEndsTheAutomaticSignInEvenIfTheKeyringKeepsTheToken
// holds the same for the MyGo client.
func TestAKeyringThatKeepsTheTokenSaysSoOnTheSignInScreen(t *testing.T) {
	s := pocketdtest.New()
	ring := newTestKeyring()
	app := signInScreen(t, s.Transport(), ring)
	typeSignIn(app, pocketdtest.URL, pocketdtest.Token)
	app.Press("role:button Connect")
	app.WaitFor("label:"+paletteLabel, 5*time.Second)
	app.WaitUntil(5*time.Second, func() bool { kept, _ := app.Storage().GetItem("connected-server"); return kept == pocketdtest.URL })
	ring.failForget(errors.New("the keyring is locked"))
	app.Key("mod+k")
	app.WaitFor("text:Sign out", 5*time.Second)
	app.Press("text:Sign out")
	app.Press("role:alertdialog >> role:button Sign out")
	app.WaitFor("label:Device token", 5*time.Second)
	app.WaitUntil(5*time.Second, func() bool { return problemShown(app) != "" })
	want := "Signed out, but the keyring is locked. PocketGo will not sign in with it again; remove it with your keyring's app, or revoke the device with pocketd device revoke."
	if got := problemShown(app); !strings.HasPrefix(got, want) {
		t.Errorf("the sign-in screen says %q, want %q", got, want)
	}
	if kept, _ := app.Storage().GetItem("connected-server"); kept != "" {
		t.Errorf("the next start would sign in to %q", kept)
	}
}

// TestAKeyringThatWillNotKeepTheTokenSaysSo checks a sign-in whose token
// the keyring will not keep: the owner is signed in and told that the
// next start asks again, and nothing names the service for a sign-in at
// start. TestAFailedTokenSaveEndsTheOldAutomaticSignIn and the MyGo
// client's startSignIn hold the same.
func TestAKeyringThatWillNotKeepTheTokenSaysSo(t *testing.T) {
	s := pocketdtest.New()
	ring := newTestKeyring()
	ring.setErr = errors.New("the keyring is locked")
	app := signInScreen(t, s.Transport(), ring)
	typeSignIn(app, pocketdtest.URL, pocketdtest.Token)
	app.Press("role:button Connect")
	app.WaitFor("label:"+paletteLabel, 5*time.Second)
	app.WaitFor("role:alert >> text:"+keyringNote, 5*time.Second)
	app.Find("role:alert >> text:You will sign in again next time. The keyring said: the keyring is locked.")
	if kept, _ := app.Storage().GetItem("connected-server"); kept != "" {
		t.Errorf("the next start would sign in to %q", kept)
	}
	if last, _ := app.Storage().GetItem("last-server"); last != pocketdtest.URL {
		t.Errorf("the form will offer %q", last)
	}
	app.Screenshot("signin-keyring-note")
}
