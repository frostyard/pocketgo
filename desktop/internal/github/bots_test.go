package github

import (
	"context"
	"strings"
	"testing"
)

// The dependency bots are those of the list, by the type and login
// GitHub's GraphQL gives: an App the agents act through is a Bot too, and
// is not one (pocketgo ADR-0031, the owner's choice of 2026-11-21).
func TestDependencyBotsAreThoseOfTheList(t *testing.T) {
	for _, c := range []struct {
		typ, login string
		want       bool
	}{
		{"Bot", "dependabot", true},
		{"Bot", "Dependabot", true},
		{"Bot", "renovate", true},
		{"Bot", "frostyard-machines", false},
		{"Bot", "github-actions", false},
		{"User", "dependabot", false},
		{"User", "renovate", false},
		{"Organization", "dependabot", false},
		{"", "dependabot", false},
		{"Bot", "", false},
		{"Bot", "dependabot[bot]", false}, // GraphQL never gives a Bot's login with [bot]
		{"Bot", "dependabot-preview", false},
	} {
		if got := DependencyBot(c.typ, c.login); got != c.want {
			t.Errorf("%s %q: %v, want %v", c.typ, c.login, got, c.want)
		}
	}
}

// riskyBy is bketelsen/pi-pocket-team#238 with risk:high, on its base with
// no bypass, by the author GitHub names typ and login.
func riskyBy(t *testing.T, typ, login string) *fake {
	f := direct(t)
	f.pr()["labels"] = labels(labelReady, labelRisk)
	f.pr()["author"] = map[string]any{"__typename": typ, "login": login}
	return f
}

// Merge All Ready's list merges a dependency bot's risk:high pull request
// without its diff opened; a card's merge of it still needs the diff, and
// so does an agents' App's in the list. Each read of Act checks the author
// again (pocketgo ADR-0031, the owner's choice of 2026-11-21).
func TestTheListWaivesOpenDiffOnlyForADependencyBot(t *testing.T) {
	preview := func(t *testing.T, f *fake) (*Client, Preview) {
		t.Helper()
		c := f.client()
		p, err := c.Preview(context.Background(), f.ref, CrossReview{})
		if err != nil || !p.Eligible || !p.RiskHigh {
			t.Fatalf("preview %+v, %v", p, err)
		}
		f.reset()
		return c, p
	}

	f := riskyBy(t, "Bot", "dependabot")
	c, p := preview(t, f)
	if !p.DependencyBot {
		t.Fatalf("Dependabot's preview %+v", p)
	}
	if res := c.Act(context.Background(), Intent{Preview: p, Mode: "single", WaiveDiff: true}, "op-list", nil); res.Kind != "merged" ||
		strings.Join(f.mutations, ",") != approve+","+merge {
		t.Fatalf("the list's merge of Dependabot's: %+v, mutations %v", res, f.mutations)
	}

	f = riskyBy(t, "Bot", "dependabot")
	c, p = preview(t, f)
	if res := c.Act(context.Background(), Intent{Preview: p, Mode: "single"}, "op-card", nil); res.Kind != "refused" ||
		!strings.Contains(res.Reason, "open the diff at") || len(f.mutations) != 0 {
		t.Fatalf("a card's merge of Dependabot's: %+v, mutations %v", res, f.mutations)
	}

	f = riskyBy(t, "Bot", "frostyard-machines")
	c, p = preview(t, f)
	if p.DependencyBot {
		t.Fatalf("the agents' App's preview %+v", p)
	}
	if res := c.Act(context.Background(), Intent{Preview: p, Mode: "single", WaiveDiff: true}, "op-agent", nil); res.Kind != "refused" ||
		!strings.Contains(res.Reason, "open the diff at") || len(f.mutations) != 0 {
		t.Fatalf("the list's merge of the agents' App's: %+v, mutations %v", res, f.mutations)
	}

	// An author the later read finds otherwise is no dependency bot's.
	f = riskyBy(t, "Bot", "dependabot")
	c, p = preview(t, f)
	f.onRead = func(_ int, pr map[string]any) {
		pr["author"] = map[string]any{"__typename": "Bot", "login": "frostyard-machines"}
	}
	if res := c.Act(context.Background(), Intent{Preview: p, Mode: "single", WaiveDiff: true}, "op-moved", nil); res.Kind != "refused" ||
		len(f.mutations) != 0 {
		t.Fatalf("an author read otherwise: %+v, mutations %v", res, f.mutations)
	}

	// No author GitHub names is no dependency bot.
	f = direct(t)
	f.pr()["labels"] = labels(labelReady, labelRisk)
	f.pr()["author"] = nil
	if _, p = preview(t, f); p.DependencyBot {
		t.Fatalf("a null author's preview %+v", p)
	}
}
