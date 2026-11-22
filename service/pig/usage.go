package pig

import (
	"strings"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/durable/harness"
	"github.com/frostyard/pocketgo/proto"
)

// A usage's cost is what its provider charges per token only when the
// provider is metered: signed in with an API key, or the owner's own
// (ADR-0023). A subscription, signed in by OAuth, is paid by the month:
// PiG still prices its answers at the catalog's rates, an estimate no one
// pays, so pocketd reports their cost as 0 wherever it reports a usage,
// and a client shows a subscription's tokens alone (ADR-0052; Codex review
// of #107, round 1).

// meteredBy says a provider's usage is charged per token: its sign-in kept
// is no subscription's, or, with none kept now, PiG signs in to it with no
// OAuth flow, as pocketd login keeps an API key only for such a provider.
func meteredBy(cred ai.Credential, kept, oauthFlow bool) bool {
	if kept {
		return cred.Type != ai.CredentialOAuth
	}
	return !oauthFlow
}

// metered says provider's usage is charged per token (meteredBy); the
// owner's providers always are.
func (e *Engine) metered(provider string) bool {
	if _, own := ownIDs(e.opts.Own.Providers)[provider]; own {
		return true
	}
	var cred ai.Credential
	kept := false
	if e.auth != nil {
		var err error
		if cred, kept, err = e.auth.GetRaw(provider); err != nil {
			kept = false
		}
	}
	oauthFlow := false
	if p := e.models.GetProvider(provider); p != nil {
		oauthFlow = p.Auth.OAuth != nil
	}
	return meteredBy(cred, kept, oauthFlow)
}

// priced is u with its cost only when provider is metered.
func priced(u proto.Usage, provider string, metered func(string) bool) proto.Usage {
	if !metered(provider) {
		u.Cost = 0
	}
	return u
}

// priceEntry gives entry's usage its cost only when its model's provider
// is metered.
func (e *Engine) priceEntry(entry *proto.Entry) {
	if entry == nil || entry.Usage == nil || entry.Model == nil {
		return
	}
	u := priced(*entry.Usage, entry.Model.Provider, e.metered)
	entry.Usage = &u
}

// liveOf is viewLive with its reply's usage priced (priceEntry).
func (e *Engine) liveOf(id int64, view harness.ConversationView, queue *queueCache) proto.Live {
	live := viewLive(id, view, queue)
	e.priceEntry(live.Partial)
	return live
}

// providerOf is the provider of a usage ledger's key, "provider/model".
func providerOf(key string) string {
	provider, _, _ := strings.Cut(key, "/")
	return provider
}
