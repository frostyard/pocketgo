package pig

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/frostyard/pocketgo/proto"
)

// A subscription's answers, signed in by OAuth, cost no one per token: PiG
// prices them at the catalog's rates anyway, so pocketd reports their cost
// as 0, in a session's usage, its entries and its live reply, and keeps an
// API key's cost (ADR-0052; Codex review of #107, round 1).
func TestASubscriptionsCostIsNoSpend(t *testing.T) {
	auth, err := ai.NewAuthStorage(filepath.Join(t.TempDir(), "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.Set("anthropic", ai.Credential{Type: ai.CredentialOAuth, Access: "access", Refresh: "refresh", Expires: 1}); err != nil {
		t.Fatal(err)
	}
	if err := auth.Set("deepseek", ai.Credential{Type: ai.CredentialAPIKey, Key: "sk-test"}); err != nil {
		t.Fatal(err)
	}
	models := ai.CreateModels()
	for _, p := range ai.BuiltinProviders() {
		if p != nil {
			models.SetProvider(p)
		}
	}
	e := &Engine{auth: auth, models: models}
	for provider, want := range map[string]bool{
		"anthropic": false, // its OAuth sign-in kept
		"deepseek":  true,  // its API key kept
		// None kept: a provider PiG signs in to by OAuth is a
		// subscription's, one it does not a key's.
		"openai-codex": false,
		"faux":         true,
	} {
		if got := e.Metered(provider); got != want {
			t.Errorf("metered(%s) = %v, want %v", provider, got, want)
		}
	}
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	entry := proto.Entry{Kind: proto.EntryAssistant, Model: &opus, Usage: &proto.Usage{Input: 10, Total: 10, Cost: 1.5}}
	e.priceEntry(&entry)
	if entry.Usage.Cost != 0 || entry.Usage.Total != 10 {
		t.Errorf("a subscription's entry %+v", entry.Usage)
	}
	deepseek := proto.ModelRef{Provider: "deepseek", ID: "deepseek-v4"}
	entry = proto.Entry{Kind: proto.EntryAssistant, Model: &deepseek, Usage: &proto.Usage{Input: 10, Total: 10, Cost: 0.25}}
	e.priceEntry(&entry)
	if entry.Usage.Cost != 0.25 {
		t.Errorf("an API key's entry %+v", entry.Usage)
	}
	// The session's ledger, by "provider/model".
	ledger := harness.NewUsageState()
	ledger.Models["anthropic/claude-opus-5-5"] = ai.Usage{Input: 100, TotalTokens: 100, Cost: ai.UsageCost{Total: 3}}
	ledger.Models["deepseek/deepseek-v4"] = ai.Usage{Input: 50, TotalTokens: 50, Cost: ai.UsageCost{Total: 0.5}}
	if got := wireUsageState(&ledger, e.Metered); got.Cost != 0.5 || got.Total != 150 {
		t.Errorf("the session's usage %+v", got)
	}
	if !meteredBy(ai.Credential{Type: ai.CredentialAPIKey}, true, true) || meteredBy(ai.Credential{Type: ai.CredentialOAuth}, true, false) {
		t.Error("a kept sign-in does not decide")
	}
}

// A session's tokens count an answer whose provider reported no total:
// PiG's ledger sums a model's answers, so a total of 60 and an answer of
// 40 input tokens with none sum to a total of 60 and parts of 100 (Codex
// review of #107, round 2).
func TestASessionsTokensCountAnswersWithoutATotal(t *testing.T) {
	ledger := harness.NewUsageState()
	ledger.Models["deepseek/deepseek-v4"] = ai.Usage{Input: 100, TotalTokens: 60}
	got := wireUsageState(&ledger, func(string) bool { return true })
	if got.Tokens() != 100 || got.Total != 100 {
		t.Fatalf("the session's usage %+v counts %d tokens", got, got.Tokens())
	}
}

// A usage read takes no view of its session: a view reads its newest
// records, images included, on every report, though nothing was written
// since the last read (Codex review of #107, round 1). Only its Entries
// query after the entry asked reads the transcript.
func TestUsageEntriesTakesNoViewOfTheSession(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "engine.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "UsageEntries" {
			continue
		}
		found = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.SelectorExpr:
				if fun.Sel.Name == "Watch" {
					t.Errorf("%s: UsageEntries takes a view of its session", fset.Position(call.Pos()))
				}
			case *ast.Ident:
				if fun.Name == "viewWindow" {
					t.Errorf("%s: UsageEntries reads a window of its session", fset.Position(call.Pos()))
				}
			}
			return true
		})
	}
	if !found {
		t.Fatal("no UsageEntries in engine.go")
	}
}
