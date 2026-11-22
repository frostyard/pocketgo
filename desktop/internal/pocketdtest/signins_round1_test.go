package pocketdtest

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// The fake keeps a start's and a cancel's request id with the other
// requests', as pocketd does: one used for another request is conflict
// and changes nothing, and a start whose flow a restart lost is not_found
// (Codex review of #123, round 1).
func TestTheFakeRefusesARequestIDUsedForAnotherRequest(t *testing.T) {
	s := New()
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	flow, err := client.StartSignIn(ctx, "github-copilot", proto.StartSignIn{RequestID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	conflict := func(err error) bool {
		var e *proto.Error
		return errors.As(err, &e) && e.Status == http.StatusConflict
	}
	if _, err := client.CancelSignIn(ctx, flow.ID, proto.CancelSignIn{RequestID: "s1"}); !conflict(err) {
		t.Fatalf("a cancel with the start's id: %v", err)
	}
	if now, _ := client.SignInFlow(ctx, flow.ID); now.State != proto.FlowWaiting {
		t.Fatalf("the refused cancel left %+v", now)
	}
	if _, err := client.CancelSignIn(ctx, flow.ID, proto.CancelSignIn{RequestID: "c1"}); err != nil {
		t.Fatal(err)
	}
	second, _ := client.StartSignIn(ctx, "github-copilot", proto.StartSignIn{RequestID: "s2"})
	if _, err := client.CancelSignIn(ctx, second.ID, proto.CancelSignIn{RequestID: "c1"}); !conflict(err) {
		t.Fatalf("a cancel id reused for another flow: %v", err)
	}
	if _, err := client.SignOut(ctx, "github-copilot"); err != nil {
		t.Fatal(err)
	}
	s.ForgetSignInFlows()
	var e *proto.Error
	if _, err := client.StartSignIn(ctx, "github-copilot", proto.StartSignIn{RequestID: "s2"}); !errors.As(err, &e) || e.Status != http.StatusNotFound {
		t.Fatalf("a start whose flow is gone: %v", err)
	}
}

// A sign-out takes the provider's models, as pocketd lists them, and
// leaves the others' (Codex review of #123, round 1).
func TestTheFakesSignOutTakesTheProvidersModels(t *testing.T) {
	s := New()
	opus := proto.Model{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}}
	s.AddModel(opus)
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := client.StartSignIn(ctx, "github-copilot", proto.StartSignIn{RequestID: "s1"}); err != nil {
		t.Fatal(err)
	}
	s.EndSignIn("github-copilot", proto.FlowApproved, "octocat", "", proto.Model{ModelRef: proto.ModelRef{Provider: "github-copilot", ID: "gpt-5"}})
	if models, _ := client.Models(ctx); len(models) != 2 {
		t.Fatalf("the models after the sign-in %+v", models)
	}
	if _, err := client.SignOut(ctx, "github-copilot"); err != nil {
		t.Fatal(err)
	}
	if models, _ := client.Models(ctx); len(models) != 1 || models[0].ModelRef != opus.ModelRef {
		t.Fatalf("the models after the sign-out %+v", models)
	}
}

// A second account's approval replaces the provider's models, as pocketd
// lists the new account's alone (Codex review of #123, round 2).
func TestTheFakesNextAccountReplacesTheProvidersModels(t *testing.T) {
	s := New()
	opus := proto.Model{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}}
	s.AddModel(opus)
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i, id := range []string{"gpt-5", "gpt-6"} {
		if _, err := client.StartSignIn(ctx, "github-copilot", proto.StartSignIn{RequestID: id}); err != nil {
			t.Fatal(err)
		}
		s.EndSignIn("github-copilot", proto.FlowApproved, "account"+id, "", proto.Model{ModelRef: proto.ModelRef{Provider: "github-copilot", ID: id}})
		models, _ := client.Models(ctx)
		if len(models) != 2 || models[0].ModelRef != opus.ModelRef || models[1].ID != id {
			t.Fatalf("the models after approval %d: %+v", i+1, models)
		}
	}
}
