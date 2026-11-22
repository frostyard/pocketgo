package api

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/signins"
)

// failRecordOnce makes the next record of kind fail.
func failRecordOnce(sh *signInHarness, kind string) {
	var once sync.Once
	sh.server.signIns.record = func(ctx context.Context, key, k, flow string) error {
		failed := false
		if k == kind {
			once.Do(func() { failed = true })
		}
		if failed {
			return errors.New("the work database refused the write")
		}
		return sh.store.RecordRequest(ctx, key, k, flow)
	}
}

// A start whose record failed still answers its flow to a retry with the
// same request id, once that flow ended too, and never starts a second
// one (Codex review of #123, round 1).
func TestAStartWhoseRecordFailedAnswersItsFlowAgain(t *testing.T) {
	sh := newSignInHarness(t)
	ctx := context.Background()
	failRecordOnce(sh, startSignInKind+signins.Copilot)
	_, err := sh.client.StartSignIn(ctx, signins.Copilot, proto.StartSignIn{RequestID: "s1"})
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	flow, ok := sh.server.signIns.flows.Latest(signins.Copilot)
	if !ok || flow.State != proto.FlowWaiting {
		t.Fatalf("the started flow %+v %v", flow, ok)
	}
	if _, err := sh.client.CancelSignIn(ctx, flow.ID, proto.CancelSignIn{RequestID: "c1"}); err != nil {
		t.Fatal(err)
	}
	again, err := sh.client.StartSignIn(ctx, signins.Copilot, proto.StartSignIn{RequestID: "s1"})
	if err != nil || again.ID != flow.ID || again.State != proto.FlowCanceled {
		t.Fatalf("the retry answered %+v %v, want the first flow, canceled", again, err)
	}
	if latest, _ := sh.server.signIns.flows.Latest(signins.Copilot); latest.ID != flow.ID {
		t.Fatalf("the retry started %s", latest.ID)
	}
}

// A cancel whose record failed cancels nothing, and its request id then
// names its flow alone (Codex review of #123, round 1).
func TestACancelWhoseRecordFailedCancelsNothing(t *testing.T) {
	sh := newSignInHarness(t)
	ctx := context.Background()
	flow, err := sh.client.StartSignIn(ctx, signins.Copilot, proto.StartSignIn{RequestID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	failRecordOnce(sh, cancelSignInKind)
	_, err = sh.client.CancelSignIn(ctx, flow.ID, proto.CancelSignIn{RequestID: "c1"})
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	if now, _ := sh.client.SignInFlow(ctx, flow.ID); now.State != proto.FlowWaiting {
		t.Fatalf("a cancel whose record failed left %+v", now)
	}
	if canceled, err := sh.client.CancelSignIn(ctx, flow.ID, proto.CancelSignIn{RequestID: "c1"}); err != nil || canceled.State != proto.FlowCanceled {
		t.Fatalf("the retry %+v %v", canceled, err)
	}
	second, err := sh.client.StartSignIn(ctx, signins.Copilot, proto.StartSignIn{RequestID: "s2"})
	if err != nil || second.ID == flow.ID {
		t.Fatalf("a new start %+v %v", second, err)
	}
	_, err = sh.client.CancelSignIn(ctx, second.ID, proto.CancelSignIn{RequestID: "c1"})
	asError(t, err, http.StatusConflict, proto.ErrorCodeConflict)
	if now, _ := sh.client.SignInFlow(ctx, second.ID); now.State != proto.FlowWaiting {
		t.Fatalf("a reused cancel id canceled %+v", now)
	}
}
