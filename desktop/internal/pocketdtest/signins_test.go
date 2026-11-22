package pocketdtest

import (
	"context"
	"net/http"
	"testing"

	"github.com/frostyard/pocketgo/proto"
)

// The fake runs a device flow as pocketd does (ADR-0062, protocol rule
// 69): a start answers a waiting flow with its code, a second start and a
// retry answer the same one, a provider signed in on the service host is
// refused, a test's approval signs the provider in and adds its models,
// and a sign-out and a cancel end what waits.
func TestTheFakeRunsADeviceFlow(t *testing.T) {
	s := New()
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	list, err := client.SignIns(ctx)
	if err != nil || len(list) != 3 || list[0].Provider != "github-copilot" || !list[0].DeviceFlow {
		t.Fatalf("the default sign-ins %+v %v", list, err)
	}
	flow, err := client.StartSignIn(ctx, "github-copilot", proto.StartSignIn{RequestID: "s1"})
	if err != nil || flow.State != proto.FlowWaiting || flow.UserCode != UserCode {
		t.Fatalf("StartSignIn %+v %v", flow, err)
	}
	if again, err := client.StartSignIn(ctx, "github-copilot", proto.StartSignIn{RequestID: "s2"}); err != nil || again.ID != flow.ID {
		t.Fatalf("a second start %+v %v", again, err)
	}
	if _, err := client.StartSignIn(ctx, "anthropic", proto.StartSignIn{RequestID: "s3"}); err == nil {
		t.Fatal("anthropic started a device flow")
	}
	gpt := proto.Model{ModelRef: proto.ModelRef{Provider: "github-copilot", ID: "gpt-5"}}
	if !s.EndSignIn("github-copilot", proto.FlowApproved, "octocat", "", gpt) || s.EndSignIn("github-copilot", proto.FlowDenied, "", "") {
		t.Fatal("EndSignIn")
	}
	if got, err := client.StartSignIn(ctx, "github-copilot", proto.StartSignIn{RequestID: "s1"}); err != nil || got.State != proto.FlowApproved || got.Account != "octocat" {
		t.Fatalf("the retry of the first start %+v %v", got, err)
	}
	if models, _ := client.Models(ctx); len(models) != 1 || models[0].ModelRef != gpt.ModelRef {
		t.Fatalf("the models after the sign-in %+v", models)
	}
	next, _ := client.StartSignIn(ctx, "github-copilot", proto.StartSignIn{RequestID: "s4"})
	if next.ID == flow.ID {
		t.Fatal("a start after the end answered the ended flow")
	}
	if canceled, err := client.CancelSignIn(ctx, next.ID, proto.CancelSignIn{RequestID: "c1"}); err != nil || canceled.State != proto.FlowCanceled {
		t.Fatalf("CancelSignIn %+v %v", canceled, err)
	}
	if out, err := client.SignOut(ctx, "github-copilot"); err != nil || out.State != proto.SignInNone || out.Account != "" {
		t.Fatalf("SignOut %+v %v", out, err)
	}
}
