package api

import (
	"context"
	"errors"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/signins"
)

// A start's request is in the work database, naming its flow, before the
// flow is published or polls: a stop after it leaves a request a retry is
// told is gone, never one that starts a second flow (Codex review of #123,
// round 2).
func TestAStartIsRecordedBeforeItsFlowIsSeen(t *testing.T) {
	sh := newSignInHarness(t)
	ctx := context.Background()
	key := requestKeyOf(sh.harness, "s1")
	seen := make(chan string, 4)
	emit := sh.server.emit
	sh.server.emit = func(e proto.Event) {
		if e.Type == proto.EventSignIn && e.SignIn.Flow != nil && e.SignIn.Flow.State == proto.FlowWaiting {
			result, existed, err := sh.store.LookupRequest(ctx, key, startSignInKind+signins.Copilot)
			if err != nil || !existed {
				result = "no record"
			}
			seen <- result + " " + e.SignIn.Flow.ID
		}
		emit(e)
	}
	flow, err := sh.client.StartSignIn(ctx, signins.Copilot, proto.StartSignIn{RequestID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != flow.ID+" "+flow.ID {
		t.Fatalf("at the flow's event the request was %q, want it to name %s", got, flow.ID)
	}

	// A record of an earlier run, whose flow is gone: not_found, and no
	// flow starts.
	if err := sh.store.RecordRequest(ctx, requestKeyOf(sh.harness, "old"), startSignInKind+signins.Copilot, signins.NewID()); err != nil {
		t.Fatal(err)
	}
	if _, err := sh.client.CancelSignIn(ctx, flow.ID, proto.CancelSignIn{RequestID: "c1"}); err != nil {
		t.Fatal(err)
	}
	_, err = sh.client.StartSignIn(ctx, signins.Copilot, proto.StartSignIn{RequestID: "old"})
	var e *proto.Error
	if !errors.As(err, &e) || e.Code != proto.ErrorCodeNotFound {
		t.Fatalf("a start whose flow is gone: %v", err)
	}
	if latest, _ := sh.server.signIns.flows.Latest(signins.Copilot); latest.ID != flow.ID {
		t.Fatalf("a start whose flow is gone started %s", latest.ID)
	}
}
