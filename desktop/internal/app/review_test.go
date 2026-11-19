package app

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// TestAFailedTokenSaveEndsTheOldAutomaticSignIn starts with an automatic
// sign-in to another service that the keyring could not be read for, then
// signs in to a new one whose token the keyring will not keep: the next
// start signs in to neither.
func TestAFailedTokenSaveEndsTheOldAutomaticSignIn(t *testing.T) {
	x := newTestApp(t)
	x.a.settings.Server, x.a.settings.LastSession = "https://old.example.ts.net:8443", 9
	x.ring.setErr = errors.New("the keyring is locked")
	x.a.signIn.server, x.a.signIn.token = x.srv.URL, pocketdtest.Token
	x.settle()
	x.click(t, "Connect")
	x.until(t, func() bool { return x.a.st.Conn.State == state.Connected })
	st, _ := x.store.Load()
	if st.Server != "" || st.LastSession != 0 {
		t.Errorf("settings %+v: the next start would sign in by itself", st)
	}
	if st.LastServer != x.srv.URL {
		t.Errorf("the form will offer %q", st.LastServer)
	}
}

func TestSignOutProblemSaysWhatWasLeft(t *testing.T) {
	saveErr, forgetErr := errors.New("disk full"), errors.New("the keyring is locked")
	if got := signOutProblem(nil, nil); got != "" {
		t.Errorf("a clean sign-out says %q", got)
	}
	both := signOutProblem(saveErr, forgetErr)
	if !strings.Contains(both, "may sign in again") || strings.Contains(both, "will not sign in") {
		t.Errorf("both steps failed, and it says %q", both)
	}
	if got := signOutProblem(nil, forgetErr); !strings.Contains(got, "will not sign in with it again") {
		t.Errorf("only the keyring failed, and it says %q", got)
	}
	if got := signOutProblem(saveErr, nil); !strings.Contains(got, "cannot sign in by itself") {
		t.Errorf("only the settings failed, and it says %q", got)
	}
}

// TestAStaleTranscriptSaysSo fails the open session's refetch, as after a
// resync: the transcript stays, and says it may be out of date.
func TestAStaleTranscriptSaysSo(t *testing.T) {
	x := newTestApp(t)
	x.seed()
	x.connect(t)
	x.click(t, "Fix the flaky CI in nsl, waiting")
	x.until(t, func() bool { return x.a.st.Data.Open() != nil && x.a.st.Data.Open().Loaded && x.a.st.InFlight() == 0 })
	x.srv.FailOnce(http.MethodGet, "/v1/sessions/1", http.StatusBadGateway)
	x.a.st.Refresh()
	x.until(t, func() bool { return x.a.st.Data.Open().Err != "" && x.a.st.InFlight() == 0 })
	x.hasAll(t, "Try Again", "The arm64 job fails about once a day. Find out why and fix it.")
	if !strings.Contains(strings.Join(x.tt.Texts(), " "), "may be out of date") {
		t.Errorf("no stale notice in %q", x.tt.Texts())
	}
	x.click(t, "Try Again")
	x.until(t, func() bool { return x.a.st.Data.Open().Err == "" && x.a.st.InFlight() == 0 })
	if strings.Contains(strings.Join(x.tt.Texts(), " "), "may be out of date") {
		t.Error("the notice stays after the transcript loaded")
	}
}

// TestTheChosenModelStaysByIdentity reorders two models of one name: the
// label the owner chose now names the other model, and the choice follows
// the model, not the label.
func TestTheChosenModelStaysByIdentity(t *testing.T) {
	a := proto.Model{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "a"}, Name: "Opus"}
	b := proto.Model{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "b"}, Name: "Opus"}
	f := &newSessionForm{}
	f.modelChoices([]proto.Model{a, b})
	if f.chosen() != a.ModelRef {
		t.Fatalf("first choice %+v", f.chosen())
	}
	f.modelChoices([]proto.Model{b, a})
	if f.chosen() != a.ModelRef {
		t.Errorf("after the reorder the form sends %+v; the owner chose %+v", f.chosen(), a.ModelRef)
	}
	f.modelChoices([]proto.Model{b})
	if f.chosen() != b.ModelRef {
		t.Errorf("with the chosen model gone the form sends %+v", f.chosen())
	}
}

// TestTheThinkingSelectOffersTheChosenModelsLevels offers the thinking
// levels the chosen model lists (protocol rule 8), and takes a level back
// to the default when the next model lacks it.
func TestTheThinkingSelectOffersTheChosenModelsLevels(t *testing.T) {
	opus := proto.Model{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "opus"}, Name: "Opus", Reasoning: true, ThinkingLevels: []string{"off", "low", "high", "xhigh"}}
	plain := proto.Model{ModelRef: proto.ModelRef{Provider: "openai", ID: "plain"}, Name: "Plain", ThinkingLevels: []string{"off"}}
	f := &newSessionForm{thinking: defaultThinking}
	f.modelChoices([]proto.Model{opus, plain})
	if got := strings.Join(f.thinkingChoices(), ","); got != "default,off,low,high,xhigh" {
		t.Errorf("Opus offers %q", got)
	}
	f.thinking = "xhigh"
	f.model = f.labels()[1]
	if got := strings.Join(f.thinkingChoices(), ","); got != "default,off" || f.thinking != defaultThinking {
		t.Errorf("Plain offers %q with %q chosen; want default,off with the default", got, f.thinking)
	}
}

// TestAFailedModelReloadShowsWithTheOldChoices fails the reload of a dialog
// that has models from before: it says so, and offers Reload Models.
func TestAFailedModelReloadShowsWithTheOldChoices(t *testing.T) {
	x := newTestApp(t)
	x.connect(t)
	x.click(t, "New Session")
	x.until(t, func() bool { m, _ := x.a.st.Models(); return len(m) == 2 && x.a.st.InFlight() == 0 })
	x.click(t, "Cancel")
	x.srv.FailOnce(http.MethodGet, proto.PathModels, http.StatusBadGateway)
	x.click(t, "New Session")
	x.until(t, func() bool { _, err := x.a.st.Models(); return err != "" && x.a.st.InFlight() == 0 })
	x.hasAll(t, "Reload Models", "Claude Opus 5.5 · anthropic")
	if !strings.Contains(strings.Join(x.tt.Texts(), " "), "Reloading the models failed") {
		t.Errorf("no failure in %q", x.tt.Texts())
	}
	x.click(t, "Reload Models")
	x.until(t, func() bool { _, err := x.a.st.Models(); return err == "" && x.a.st.InFlight() == 0 })
	if x.tt.HasText("Reload Models") {
		t.Error("Reload Models stays after the models loaded")
	}
}
