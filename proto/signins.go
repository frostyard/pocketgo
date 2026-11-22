package proto

import "time"

// SignInState is what pocketd holds for a provider's sign-in (ADR-0062,
// rule 69), as its one credential store has it.
type SignInState string

const (
	// SignInNone: no sign-in pocketd runs on.
	SignInNone SignInState = "none"
	// SignInSignedIn: a subscription's sign-in pocketd runs on, and
	// refreshes itself as it needs.
	SignInSignedIn SignInState = "signed_in"
	// SignInExpired: a subscription's sign-in whose token has expired and
	// that holds nothing to refresh it with: sign in again.
	SignInExpired SignInState = "expired"
)

// SignIn is a model provider whose subscription pocketd signs in to:
// GET /v1/signins lists one for each, in the service's order, GitHub
// Copilot first, and a signin event carries one (ADR-0062, rule 69). It
// never carries a token.
type SignIn struct {
	Provider string      `json:"provider"`
	Name     string      `json:"name"`
	State    SignInState `json:"state"`
	// Account is the account the sign-in is for, as the provider named it
	// when pocketd signed in from a device flow: a GitHub login for
	// github-copilot. "" when pocketd does not know it, as for a sign-in
	// made with pocketd login.
	Account string `json:"account,omitempty"`
	// DeviceFlow says a client may sign pocketd in to the provider (POST
	// /v1/signins/{id}) and out of it (DELETE). Another provider
	// signs in on the service host alone, with pocketd login.
	DeviceFlow bool `json:"deviceFlow,omitempty"`
	// Flow is the provider's newest device flow since pocketd started,
	// running or ended; nil for none.
	Flow *SignInFlow `json:"flow,omitempty"`
}

// SignInFlowState is where a device flow stands (rule 69).
type SignInFlowState string

const (
	// FlowWaiting: the code waits for the owner's approval at the
	// verification page, and pocketd polls the provider.
	FlowWaiting SignInFlowState = "waiting"
	// FlowApproved: the owner approved, and pocketd keeps the sign-in.
	FlowApproved SignInFlowState = "approved"
	// FlowDenied: the owner denied the code at the verification page.
	FlowDenied SignInFlowState = "denied"
	// FlowExpired: the code expired before it was approved.
	FlowExpired SignInFlowState = "expired"
	// FlowCanceled: a client canceled the flow, or a sign-out did.
	FlowCanceled SignInFlowState = "canceled"
	// FlowFailed: the provider could not be reached, answered what pocketd
	// does not take, or pocketd could not keep the sign-in; Error says
	// which.
	FlowFailed SignInFlowState = "failed"
)

// Ended says the flow ended: every state but waiting.
func (s SignInFlowState) Ended() bool { return s != FlowWaiting }

// SignInFlow is a device flow pocketd runs to sign in to a provider
// (ADR-0062, rule 69). It never carries a token, nor the device code.
type SignInFlow struct {
	ID       string          `json:"id"`
	Provider string          `json:"provider"`
	State    SignInFlowState `json:"state"`
	// UserCode is the code the owner enters at VerificationURI, both as
	// the provider gave them; empty for a flow that failed before the
	// provider gave a code.
	UserCode        string `json:"userCode,omitempty"`
	VerificationURI string `json:"verificationUri,omitempty"`
	// ExpiresIn is how many seconds the code holds from StartedAt, as the
	// provider said, and Interval how many seconds pocketd waits between
	// polls now: a slow_down widens it.
	ExpiresIn int       `json:"expiresIn,omitempty"`
	Interval  int       `json:"interval,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	// ExpiresAt is when the code stops holding, by the service's clock:
	// StartedAt plus ExpiresIn. A client shows the time left from it.
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
	// EndedAt is when the flow ended; zero while it waits.
	EndedAt time.Time `json:"endedAt,omitzero"`
	// Account is the account an approved flow signed in, when the provider
	// named it: a GitHub login.
	Account string `json:"account,omitempty"`
	// Error says why a denied, expired or failed flow ended.
	Error string `json:"error,omitempty"`
}

// StartSignIn is the body of POST /v1/signins/{id}: a device flow
// starts, or the provider's running one answers (rule 69).
type StartSignIn struct {
	RequestID string `json:"requestId"`
}

// CancelSignIn is the body of POST /v1/signins/flows/{id}/cancel.
type CancelSignIn struct {
	RequestID string `json:"requestId"`
}
