// Package signins holds the rules of the sign-ins pocketd makes from a
// client (ADR-0062): the providers it knows, what a stored sign-in's state
// is, GitHub's device flow, and the flows it runs, polled in code. A
// GitHub token lives here only from the poll that gets it to the Keep that
// stores it through the service's one credential writer; nothing here
// logs, answers or publishes one.
package signins

import "github.com/frostyard/pocketgo/proto"

// Provider is a model provider whose subscription pocketd signs in to.
type Provider struct {
	ID, Name string
	// Device says pocketd signs in to it with a device flow it runs
	// itself (Flows), so a client may start one, and sign out.
	Device bool
}

// Copilot is GitHub Copilot's provider id, as PiG names it.
const Copilot = "github-copilot"

// Providers are the subscriptions pocketd signs in to (ADR-0005), in the
// order a client lists them: GitHub Copilot first. Copilot alone signs in
// with a device flow pocketd runs (ADR-0062); the others sign in on the
// service host, with pocketd login, as their flows differ (a browser
// redirect and a pasted code). Another goes here with its test.
var Providers = []Provider{
	{ID: Copilot, Name: "GitHub Copilot", Device: true},
	{ID: "anthropic", Name: "Anthropic (Claude)"},
	{ID: "openai-codex", Name: "OpenAI Codex (ChatGPT)"},
}

// Find is the provider named id.
func Find(id string) (Provider, bool) {
	for _, p := range Providers {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

// Stored is what the credential store holds for a provider, with no
// secret in it: what State decides from.
type Stored struct {
	// Kind is the credential's type, as the store names it: "oauth" for a
	// subscription's sign-in, "api_key" for a key, "" for none.
	Kind string
	// Refreshable says it holds a refresh token; Expired says the expiry of
	// its access token has passed.
	Refreshable, Expired bool
	// Identity names the credential without a secret, "" for none: an
	// account pocketd kept is shown only beside the credential it was kept
	// with (Account).
	Identity string
}

// Credential kinds, as PiG's credential store names them.
const (
	KindOAuth  = "oauth"
	KindAPIKey = "api_key"
)

// State is a stored sign-in's state (rule 69). A subscription's sign-in
// (OAuth) is signed in, or expired once its access token expired with no
// refresh token to renew it: pocketd refreshes the others itself, and
// cannot tell a refresh token the provider revoked until it uses it.
// Anything else is none, an API key included: pocketd lists no model of a
// subscription's provider on a key (ADR-0022).
func State(s Stored) proto.SignInState {
	switch {
	case s.Kind != KindOAuth:
		return proto.SignInNone
	case s.Expired && !s.Refreshable:
		return proto.SignInExpired
	}
	return proto.SignInSignedIn
}

// Account is the account to show beside a stored sign-in: the one kept
// with that same credential (identity kept), "" for another or none. A
// sign-in made since with pocketd login, or a refreshed one whose identity
// changed, shows none rather than another's.
func Account(s Stored, kept, account string) string {
	if State(s) == proto.SignInNone || s.Identity == "" || kept != s.Identity {
		return ""
	}
	return account
}
