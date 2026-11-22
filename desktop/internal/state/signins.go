package state

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// The owner signs pocketd in to GitHub Copilot from Settings' Sign-ins page
// (ADR-0062): pocketd runs GitHub's device flow, and the sheet shows its
// code, sends the owner to GitHub's device page, waits for the approval
// and says how it ended. The desktop never sees or keeps a token, and
// opens only the device page it builds (DevicePage), never a URL pocketd
// sent.

// signInData is what the client knows of pocketd's sign-ins: the list of
// the newest answer, each provider's row replaced by its signin event,
// which wins over the copy of a fetch begun before it.
type signInData struct {
	rows    []proto.SignIn
	list    list
	touched map[string]uint64
}

// SignIns are pocketd's sign-ins, in its order, GitHub Copilot first. The
// slice is the Data's own: read it, do not change it.
func (d *Data) SignIns() []proto.SignIn { return d.signIns.rows }

// SignInsStatus is where the sign-ins' list stands, and why its last fetch
// failed.
func (d *Data) SignInsStatus() (ListStatus, string) {
	return d.signIns.list.status, d.signIns.list.err
}

// SignIn is provider's sign-in, as the client knows it.
func (d *Data) SignIn(provider string) (proto.SignIn, bool) {
	for _, row := range d.signIns.rows {
		if row.Provider == provider {
			return row, true
		}
	}
	return proto.SignIn{}, false
}

// SetSignIns records fetch f's sign-ins, unless a fetch begun later
// already answered; a provider whose event came since f began keeps the
// event's row.
func (d *Data) SetSignIns(f Fetch, rows []proto.SignIn) {
	if !d.signIns.list.answer(f) {
		return
	}
	d.rev++
	d.signIns.list.status, d.signIns.list.err = ListLoaded, ""
	out := make([]proto.SignIn, 0, len(rows))
	for _, row := range rows {
		if newer(d.signIns.touched[row.Provider], f) {
			if kept, ok := d.SignIn(row.Provider); ok {
				row = kept
			}
		}
		out = append(out, row)
	}
	d.signIns.rows = out
}

// SetSignInsError records that the sign-ins' fetch f failed, unless a
// fetch begun later already answered; the rows stay.
func (d *Data) SetSignInsError(f Fetch, err string) {
	if d.signIns.list.answer(f) {
		d.rev++
		d.signIns.list.status, d.signIns.list.err = ListFailed, err
	}
}

// applySignIn takes a signin event: the provider's row, whole. It says
// whether the models pocketd runs may have changed (modelsMoved): a
// provider the client did not know counts as changed.
func (d *Data) applySignIn(row proto.SignIn) (changed bool) {
	if d.signIns.touched == nil {
		d.signIns.touched = map[string]uint64{}
	}
	d.signIns.touched[row.Provider] = d.touched()
	i := slices.IndexFunc(d.signIns.rows, func(r proto.SignIn) bool { return r.Provider == row.Provider })
	if i < 0 {
		d.signIns.rows = append(d.signIns.rows, row)
		return true
	}
	changed = modelsMoved(d.signIns.rows[i], row)
	d.signIns.rows[i] = row
	return changed
}

// modelsMoved says whether the models pocketd runs may have changed from
// row before to row after: the provider's state changed, or a flow was
// approved since, whose account offers models of its own, though the
// provider was signed in before and after (Codex review of #123, round 1).
func modelsMoved(before, after proto.SignIn) bool {
	if before.State != after.State {
		return true
	}
	approved := func(r proto.SignIn) string {
		if r.Flow != nil && r.Flow.State == proto.FlowApproved {
			return r.Flow.ID
		}
		return ""
	}
	return approved(after) != "" && approved(after) != approved(before)
}

// DevicePage is the page the owner enters provider's code at, which the
// sheet opens and shows: GitHub's device page for GitHub Copilot, the
// only provider a client signs in to (protocol rule 69). The desktop opens
// only this page, never a URL pocketd sent.
func DevicePage(provider string) (url, shown string, ok bool) {
	if provider == CopilotProvider {
		return "https://github.com/login/device", "github.com/login/device", true
	}
	return "", "", false
}

// CopilotProvider is GitHub Copilot's provider id.
const CopilotProvider = "github-copilot"

// SignInCaption is what the Sign-ins page says under a provider's name:
// its state, the account, and, for a provider the desktop does not sign
// in to, where it is signed in.
func SignInCaption(row proto.SignIn) string {
	var text string
	switch row.State {
	case proto.SignInSignedIn:
		text = "Signed in"
		if row.Account != "" {
			text += " as @" + row.Account
		}
	case proto.SignInExpired:
		text = "The sign-in expired: sign in again"
	default:
		text = "Not signed in"
	}
	if !row.DeviceFlow {
		text += ". Sign in on the service host: pocketd login " + row.Provider
	}
	return text
}

// SheetPhase is where the sign-in sheet stands.
type SheetPhase int

const (
	// SheetClosed: no sheet shows.
	SheetClosed SheetPhase = iota
	// SheetStarting: the start is on its way to pocketd.
	SheetStarting
	// SheetCode: the code waits for the owner's approval at the device
	// page.
	SheetCode
	// SheetSignedIn: pocketd has the sign-in.
	SheetSignedIn
	// SheetFailed: the flow ended without a sign-in, or its start failed;
	// Try Again starts another.
	SheetFailed
)

// signInSheet is the sheet of a device flow the owner started from the
// Sign-ins page: the provider, and the flow its start answered.
type signInSheet struct {
	provider string
	flow     proto.SignInFlow
	start    Pending
	// closed says the owner closed the sheet while its start was on its
	// way: it shows no more, and the flow the start answers is canceled,
	// unless the owner opens it again first.
	closed bool
	// checking says the sheet asks pocketd for its flow, which the
	// provider's row no longer names, and lost that pocketd answered it
	// keeps it no more (settleSheet).
	checking, lost bool
}

// SheetView is the sign-in sheet as the window draws it.
type SheetView struct {
	Phase SheetPhase
	// ProviderID is the provider's id, and Provider its name.
	ProviderID, Provider string
	Flow                 proto.SignInFlow
	// Text says how the flow ended: the account signed in, or why it
	// failed.
	Text string
	// Left is the code's time left, by the desktop's clock.
	Left time.Duration
}

// SignInSheet is the sign-in sheet as it stands now: the flow as the
// provider's row has it once an event or a fetch brought it, else as the
// start answered it.
func (s *Store) SignInSheet() SheetView {
	sh := s.sheet
	if sh == nil || sh.closed {
		return SheetView{}
	}
	v := SheetView{ProviderID: sh.provider, Provider: sh.provider}
	row, known := s.Data.SignIn(sh.provider)
	if known {
		v.Provider = row.Name
	}
	switch {
	case sh.start.Busy:
		v.Phase = SheetStarting
		return v
	case sh.start.Err != "":
		v.Phase, v.Text = SheetFailed, "pocketd did not start the sign-in: "+sh.start.Err
		return v
	}
	v.Flow = sh.flow
	if known && row.Flow != nil && row.Flow.ID == sh.flow.ID {
		v.Flow = *row.Flow
	}
	if sh.lost && !v.Flow.State.Ended() {
		v.Phase, v.Text = SheetFailed, "pocketd no longer runs this sign-in: it may have restarted. Try again."
		return v
	}
	v.Phase, v.Text = sheetPhase(v.Flow, v.Provider)
	if v.Phase == SheetCode {
		v.Left = max(0, v.Flow.ExpiresAt.Sub(s.now()))
	}
	return v
}

// sheetPhase is the phase a flow shows in, and what it says once ended.
func sheetPhase(f proto.SignInFlow, name string) (SheetPhase, string) {
	switch f.State {
	case proto.FlowWaiting:
		return SheetCode, ""
	case proto.FlowApproved:
		text := "Signed in"
		if f.Account != "" {
			text += " as @" + f.Account
		}
		return SheetSignedIn, text + ". " + name + " models are now available."
	case proto.FlowDenied:
		return SheetFailed, "The code was denied on GitHub."
	case proto.FlowExpired:
		return SheetFailed, "The code expired before it was approved."
	case proto.FlowCanceled:
		return SheetFailed, "The sign-in was canceled."
	}
	if f.Error != "" {
		return SheetFailed, f.Error
	}
	return SheetFailed, "The sign-in failed."
}

// FormatLeft is a code's time left as the sheet shows it: minutes and
// seconds, "4:05".
func FormatLeft(d time.Duration) string {
	secs := int(d.Round(time.Second) / time.Second)
	return fmt.Sprintf("%d:%02d", secs/60, secs%60)
}

// LoadSignIns fetches pocketd's sign-ins (GET /v1/signins): the Sign-ins
// page asks for them as Settings opens, and the store again after a
// resync or a reconnect from then on.
func (s *Store) LoadSignIns() {
	s.signInsWanted = true
	f := s.Data.Begin()
	var rows []proto.SignIn
	s.do("Loading the sign-ins", func(ctx context.Context, c *proto.Client) (err error) {
		rows, err = c.SignIns(ctx)
		return err
	}, s.track("signins", func(err error) {
		if err != nil {
			s.Data.SetSignInsError(f, err.Error())
			return
		}
		s.Data.SetSignIns(f, rows)
		s.settleSheet()
	}))
}

// WantSignIns fetches the sign-ins once, for a view that shows them (the
// compact window's More), from then on as LoadSignIns does.
func (s *Store) WantSignIns() {
	if !s.signInsWanted {
		s.LoadSignIns()
	}
}

// signInKey is the request ids' key of provider's starts, and
// cancelSignInKey that of flow id's cancels.
func signInKey(provider string) string { return "signin:" + provider }
func cancelSignInKey(id string) string { return "signin-cancel:" + id }

// StartSignIn opens the sheet on provider and starts its device flow
// (POST /v1/signins/{id}): pocketd answers the flow, or the
// provider's running one. A start that failed keeps its request id, so
// trying it again answers the flow the first try may have started; one
// that succeeded lets it go, so Try Again after an end starts another.
func (s *Store) StartSignIn(provider string) {
	// A start on its way shows again, closed or not: its answer is the
	// flow a new start would answer.
	if sh := s.sheet; sh != nil && sh.provider == provider && sh.start.Busy {
		sh.closed = false
		return
	}
	sh := &signInSheet{provider: provider}
	s.sheet = sh
	key := signInKey(provider)
	request := proto.StartSignIn{RequestID: s.ids.For(key, "start")}
	sh.start.Busy = true
	s.log(LevelCommand, "Sign pocketd in to %s", provider)
	var flow proto.SignInFlow
	s.do("Starting the sign-in to "+provider, func(ctx context.Context, c *proto.Client) (err error) {
		flow, err = c.StartSignIn(ctx, provider, request)
		return err
	}, func(err error) {
		sh.start.Busy = false
		if sh.closed && s.sheet == sh {
			s.sheet = nil
		}
		if err != nil {
			sh.start.Err = err.Error()
			// pocketd no longer has the flow this request id started (a
			// restart, or a later start): the id goes, so Try Again starts
			// another, as protocol rule 5 asks (Codex review of #123,
			// round 1). An answer that never came keeps it.
			if gone(err) {
				s.ids.Done(key)
			}
			return
		}
		s.ids.Done(key)
		sh.flow = flow
		s.settleSheet()
		// The owner closed the sheet while the start went: its flow is
		// canceled too.
		if sh.closed {
			s.cancelFlow(sh.provider, flow)
		}
	})
}

// CancelSignIn closes the sheet, and cancels its flow on pocketd while it
// waits (POST /v1/signins/flows/{id}/cancel): Cancel, Close and Escape do,
// at any step. A sheet closed while its start is on its way cancels the
// flow the start answers. A cancel that fails is logged; the code then
// expires on its own, and the next start answers the flow that still
// waits.
func (s *Store) CancelSignIn() {
	sh := s.sheet
	switch {
	case sh == nil || sh.closed:
		return
	case sh.start.Busy:
		sh.closed = true
		return
	}
	s.sheet = nil
	if sh.flow.ID != "" {
		s.cancelFlow(sh.provider, sh.flow)
	}
}

// gone says pocketd answered that what a request names is no more (404).
func gone(err error) bool {
	var e *proto.Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// settleSheet keeps the sheet's flow as the provider's row has it, so a
// later flow of the provider, another device's, never brings back an
// earlier copy of it. When the row no longer names the sheet's waiting
// flow, pocketd is asked for it (GET /v1/signins/flows/{id}): one pocketd
// no longer keeps, as after a restart, ends the sheet with Try Again
// (Codex review of #123, round 1). The store runs it after each event and
// each fetch of the sign-ins.
func (s *Store) settleSheet() {
	sh := s.sheet
	if sh == nil || sh.start.Busy || sh.flow.ID == "" || sh.lost {
		return
	}
	row, ok := s.Data.SignIn(sh.provider)
	if !ok {
		return
	}
	if row.Flow != nil && row.Flow.ID == sh.flow.ID {
		sh.flow = *row.Flow
		return
	}
	if sh.flow.State.Ended() || sh.checking {
		return
	}
	sh.checking = true
	id := sh.flow.ID
	var flow proto.SignInFlow
	s.do("Reading the sign-in to "+sh.provider, func(ctx context.Context, c *proto.Client) (err error) {
		flow, err = c.SignInFlow(ctx, id)
		return err
	}, func(err error) {
		sh.checking = false
		switch {
		case err == nil && flow.ID == sh.flow.ID:
			sh.flow = flow
		case gone(err):
			sh.lost = true
		}
	})
}

// cancelFlow cancels flow of provider on pocketd, unless it ended as the
// client knows it. Its request id is kept until a cancel succeeds.
func (s *Store) cancelFlow(provider string, flow proto.SignInFlow) {
	if row, ok := s.Data.SignIn(provider); ok && row.Flow != nil && row.Flow.ID == flow.ID {
		flow = *row.Flow
	}
	if flow.State.Ended() {
		return
	}
	key := cancelSignInKey(flow.ID)
	request := proto.CancelSignIn{RequestID: s.ids.For(key, flow.ID)}
	s.log(LevelCommand, "Cancel the sign-in to %s", provider)
	s.do("Canceling the sign-in to "+provider, func(ctx context.Context, c *proto.Client) error {
		_, err := c.CancelSignIn(ctx, flow.ID, request)
		return err
	}, func(err error) {
		if err == nil {
			s.ids.Done(key)
		}
	})
}

// SignOutOf signs pocketd out of provider (DELETE /v1/signins/{id}):
// SigningOut is where it stands. The answer is the provider's row, which
// an event that came since the request went wins over.
func (s *Store) SignOutOf(provider string) {
	p := pending(s.signOuts, provider)
	if p.Busy {
		return
	}
	p.Busy, p.Err = true, ""
	s.log(LevelCommand, "Sign pocketd out of %s", provider)
	f := s.Data.Begin()
	var row proto.SignIn
	s.do("Signing pocketd out of "+provider, func(ctx context.Context, c *proto.Client) (err error) {
		row, err = c.SignOut(ctx, provider)
		return err
	}, func(err error) {
		p.Busy = false
		if err != nil {
			p.Err = err.Error()
			return
		}
		s.Data.signedOut(f, row)
		s.LoadModels()
	})
}

// signedOut applies a sign-out's answer, unless the provider's event came
// since the request went.
func (d *Data) signedOut(f Fetch, row proto.SignIn) {
	if newer(d.signIns.touched[row.Provider], f) {
		return
	}
	d.rev++
	if i := slices.IndexFunc(d.signIns.rows, func(r proto.SignIn) bool { return r.Provider == row.Provider }); i >= 0 {
		d.signIns.rows[i] = row
	}
}

// SigningOut is where the owner's sign-out of provider stands.
func (s *Store) SigningOut(provider string) Pending { return valueOf(s.signOuts[provider]) }

// MaySignIn says whether the Sign-ins page offers Sign In… on row: a
// provider the desktop signs in to, with no sign-out of it on its way.
func (s *Store) MaySignIn(row proto.SignIn) bool {
	return row.DeviceFlow && !s.SigningOut(row.Provider).Busy
}
