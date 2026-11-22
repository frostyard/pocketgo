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

// A start whose record failed started nothing, so no flow runs that its
// request id does not name: the retry with the same id starts the flow,
// and a second retry answers that one (Codex review of #123, rounds 1
// and 2).
func TestAStartWhoseRecordFailedStartsNothing(t *testing.T) {
	sh := newSignInHarness(t)
	ctx := context.Background()
	failRecordOnce(sh, startSignInKind+signins.Copilot)
	_, err := sh.client.StartSignIn(ctx, signins.Copilot, proto.StartSignIn{RequestID: "s1"})
	asError(t, err, http.StatusInternalServerError, proto.ErrorCodeInternal)
	if flow, ok := sh.server.signIns.flows.Latest(signins.Copilot); ok {
		t.Fatalf("a start whose record failed started %+v", flow)
	}
	first, err := sh.client.StartSignIn(ctx, signins.Copilot, proto.StartSignIn{RequestID: "s1"})
	if err != nil || first.State != proto.FlowWaiting {
		t.Fatalf("the retry %+v %v", first, err)
	}
	if _, err := sh.client.CancelSignIn(ctx, first.ID, proto.CancelSignIn{RequestID: "c1"}); err != nil {
		t.Fatal(err)
	}
	again, err := sh.client.StartSignIn(ctx, signins.Copilot, proto.StartSignIn{RequestID: "s1"})
	if err != nil || again.ID != first.ID || again.State != proto.FlowCanceled {
		t.Fatalf("the second retry answered %+v %v, want the first flow, canceled", again, err)
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
