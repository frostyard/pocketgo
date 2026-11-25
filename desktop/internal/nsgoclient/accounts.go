package nsgoclient

import (
	"strconv"
	"strings"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

type credentials interface {
	Token(string) (string, error)
	SetToken(string, string) error
	Forget(string) error
}

// Serialize writes and deletes, so disconnect always takes effect after a pending save.
func (c *client) credentialJob(work func()) {
	before := c.credentialDone
	after := make(chan struct{})
	c.credentialDone = after
	go func() {
		defer close(after)
		if before != nil {
			<-before
		}
		work()
	}()
}
func (c *client) restore() {
	server, _ := c.cx.Storage().GetItem("connected-server")
	if server == "" || c.creds == nil {
		return
	}
	gen := c.generation
	c.credentialJob(func() {
		secret, err := c.creds.Token(server)
		c.cx.Post(func() {
			if gen != c.generation {
				return
			}
			if err == nil && secret != "" {
				err = c.store.Connect(server, secret)
				switch t := c.pendingNotice; {
				case err == nil && t != nil:
					// A notification clicked as the app started.
					c.pendingNotice = nil
					c.openTarget(*t)
				case err == nil && c.prefs.LastSession != 0:
					c.openSession(c.prefs.LastSession)
				}
			}
			c.result(err)
		})
	})
}
func (c *client) keep(server, token string) {
	_ = c.cx.Storage().SetItem("last-server", server)
	if c.creds == nil {
		return
	}
	gen := c.generation
	c.credentialJob(func() {
		err := c.creds.SetToken(server, token)
		c.cx.Post(func() {
			if gen != c.generation {
				return
			}
			if err != nil {
				// connected-server is not set, so no start signs in to
				// server by itself.
				c.logLine(state.LevelWarn, "Keeping the token: "+err.Error())
				c.showNotice(ui.ToastWarning, keyringNote, "You will sign in again next time. The keyring said: "+err.Error()+".")
				c.changed()
				return
			}
			_ = c.cx.Storage().SetItem("connected-server", server)
		})
	})
}
func (c *client) forget(server string) {
	c.cx.Storage().RemoveItem("connected-server")
	if c.creds != nil {
		c.credentialJob(func() {
			if err := c.creds.Forget(server); err != nil {
				c.cx.Post(func() {
					// Signed out, the sign-in screen says so, after the
					// reason pocketd gave when it signed the device out.
					if c.store.SignedIn() {
						c.result(err)
						return
					}
					c.logLine(state.LevelError, "Forgetting the token: "+err.Error())
					if p := c.signInProblem.Get(); p != "" {
						c.signInProblem.Set(p + " " + capitalize(forgetProblem(err)))
					} else {
						c.signInProblem.Set("Signed out, but " + forgetProblem(err))
					}
					c.changed()
				})
			}
		})
	}
}
func (c *client) initGitHub(auth state.GitHubAuth) {
	c.initGitHubWith(auth, func(t string) state.GitHubAPI { return github.New(t) })
}

// initGitHubWith is initGitHub with the client that acts with the token:
// github.New in the app, and the fake GitHub in a test, so that no test
// reaches the real one.
func (c *client) initGitHubWith(auth state.GitHubAuth, newClient func(string) state.GitHubAPI) {
	c.gh = state.NewGitHub(state.GitHubOptions{Post: func(fn func()) { c.cx.Post(func() { c.cx.Batch(func() { fn(); c.changed() }) }) }, Auth: auth, NewClient: newClient, Owners: []string{github.Login, "frostyard"}})
	c.gh.Open(c.changed)
}
func (c *client) githubSettings() core.Node {
	g := c.gh
	if g == nil {
		return body("Owner merge controls require the desktop keyring. Pull requests can be opened in GitHub.")
	}
	out := []core.Node{}
	phase, code, why := g.SignIn()
	out = append(out, body(strings.Join(nonempty(g.Off(), g.KeptProblem(), why), " · ")))
	if g.CanOpenSignIn() && phase != state.SignInCode && phase != state.SignInStarting && !g.Status().SignedIn {
		out = append(out, c.button("Sign in with GitHub", g.BeginSignIn))
	}
	if phase == state.SignInCode || phase == state.SignInStarting {
		out = append(out, body(code.UserCode), c.button("Open GitHub device sign-in", func() { c.cx.OpenURL("https://github.com/login/device", nil) }), c.button("Cancel GitHub sign-in", g.CancelSignIn))
	}
	if g.CanSignOut() {
		out = append(out, c.button("Sign out of GitHub", func() {
			c.destroy("Sign out of GitHub?", "Remove the owner credential from the desktop keyring.", "Sign out", func() { g.SignOut(c.result) })
		}))
	}
	if g.CanSaveAgain() {
		out = append(out, c.button("Save credential again", func() { g.SaveAgain(c.result) }))
	}
	return workspaceui.Stack(out...)
}

// pullMerge is a pull request card's approve and merge (ADR-0018), as the
// MyGo client's mergeRow says it: why approve and merge are off, or the
// GitHub sign-in they wait for, then the card's merge (pullMergeActions)
// once the sign-in holds. state.GitHub holds every rule; this only draws
// it.
func (c *client) pullMerge(p proto.PullRequest, ref github.Ref) core.Node {
	g := c.gh
	switch {
	case !g.On() && !g.Opened():
		return c.caption("Approve and merge are off: " + g.Off())
	case !g.On():
		return c.problemBanner(workspaceui.Warning, "Approve and merge are off: "+g.Off(), nil)
	case !g.Status().SignedIn:
		var out []core.Node
		if why := g.KeptProblem(); why != "" {
			out = append(out, c.problemBanner(workspaceui.Warning, why, nil))
		}
		return workspaceui.Stack(append(out, c.caption("Sign in to GitHub in Settings, under Accounts, to approve and merge it as "+github.Login+"."))...)
	}
	return c.pullMergeActions(p, ref)
}

// pullMergeActions says where the card's merge stands and offers what may
// be done next: Check merge gates reads the gates on GitHub as the owner;
// the card then says what they read, or why they refuse, and offers
// Approve and merge at the head they read, after the checked diff for a
// risk:high one, and a confirmation; it says how an attempt ended, with
// Check again.
func (c *client) pullMergeActions(p proto.PullRequest, ref github.Ref) core.Node {
	g := c.gh
	m := g.Merge(ref)
	pre := m.Preview
	var out, actions []core.Node
	if g.CanSaveAgain() {
		out = append(out, workspaceui.Banner{Tone: workspaceui.Warning, Icon: lucide.TriangleAlert, Text: "The keyring did not keep the refreshed GitHub sign-in: nothing runs on GitHub until it does.",
			Action: "Save again", OnAction: func() { g.SaveAgain(c.result); c.changed() }})
	}
	if why := g.OwnerProblem(ref.Owner); why != "" {
		out = append(out, c.problemBanner(workspaceui.Danger, "The App "+github.AppName+" cannot act on "+ref.Owner+": "+why, nil))
	}
	check := func(label string) core.Node { return c.button(label, func() { g.Check(p) }) }
	var why string
	switch m.Phase {
	case state.MergeNone:
		out = append(out, c.caption("Its merge gates are read on GitHub, as "+github.Login+"."))
		actions = append(actions, check("Check merge gates"))
	case state.MergeChecking:
		out = append(out, c.caption("Reading its merge gates on GitHub…"))
	case state.MergeActing:
		out = append(out, c.caption("Approving and merging as "+github.Login+"…"))
	case state.MergeDone:
		text, variant := mergeResult(m.Result)
		out = append(out, workspaceui.Row(ui.Badge{Text: m.Result.Kind, Variant: variant}, c.caption(text)))
		actions = append(actions, check("Check again"))
	default:
		switch {
		case m.Err != "":
			out = append(out, c.problemBanner(workspaceui.Danger, "Reading its gates failed: "+m.Err, nil))
		case !pre.Eligible:
			out = append(out, workspaceui.Row(ui.Badge{Text: "not mergeable", Variant: ui.BadgeWarn}, c.caption(pre.Reason)))
		default:
			out = append(out, c.caption(strings.Join(nonempty("Gates pass at "+shortHead(pre.Head)+" into "+pre.BaseRef, methodName(pre), pre.Note), " · ")))
		}
		// An enqueue the App may lack the permission for: it blocks nothing.
		if note := g.MergeNote(ref); note != "" {
			out = append(out, c.problemBanner(workspaceui.Warning, note, nil))
		}
		if url, can := state.DiffURL(pre); can {
			actions = append(actions, c.button("Open checked diff", func() {
				c.cx.OpenURL(url, func(opened bool) {
					if opened {
						g.OpenedDiff(ref, pre.BaseOid, pre.Head)
						c.changed()
					}
				})
			}))
		}
		actions = append(actions, check("Check again"))
		if m.Err == "" && pre.Eligible {
			allowed, off := g.CanMerge(p)
			why = off
			approve := ui.Button{Text: "Approve and merge", Disabled: func() bool { return !allowed }, OnPress: func() {
				// The base GitHub read, which the merge holds to: pocketd's
				// copy may be older.
				verb := "merges it into " + pre.BaseRef + " with " + methodName(pre)
				if pre.Queue {
					verb = "adds it to the merge queue of " + pre.BaseRef
				}
				text := "PocketGo approves it at commit " + pre.Head + " as " + github.Login + ", then " + verb + ", if the gates still pass at that head."
				if note := g.MergeNote(ref); note != "" {
					text += "\n\n" + note
				}
				c.confirm("Approve and merge "+ref.String()+"?", text, "Approve and merge", func() {
					c.store.ReadPull(p.ID, func(f state.FreshPull, found bool, err error) {
						if err != nil {
							c.showNotice(ui.ToastError, "Couldn’t merge pull request", ref.String()+": reading it from pocketd failed, nothing was sent: "+err.Error())
							return
						}
						if !found {
							c.showNotice(ui.ToastWarning, "Pull request unavailable", ref.String()+": pocketd no longer lists it; nothing was sent.")
							return
						}
						g.ApproveAndMerge(f, m, func(r github.Result) {
							title, kind := "Merge status unknown", ui.ToastWarning
							switch r.Kind {
							case "merged":
								title, kind = "Pull request merged", ui.ToastSuccess
							case "queued":
								title, kind = "Pull request queued", ui.ToastInfo
							case "approved":
								title, kind = "Approved, not merged", ui.ToastWarning
							case "refused", "failed":
								title, kind = "Couldn’t merge pull request", ui.ToastError
							}
							text, _ := mergeResult(r)
							c.showNotice(kind, title, ref.String()+": "+text)
							c.changed()
						})
					})
				})
			}}
			actions = append(actions, ui.Tooltip{Text: mergeTooltip(allowed, off), Trigger: approve})
		}
	}
	if len(actions) > 0 {
		out = append(out, workspaceui.Row(actions...))
	}
	// Why Approve and merge is off, under it.
	if why != "" {
		out = append(out, c.caption(why))
	}
	return workspaceui.Stack(out...)
}

func mergeTooltip(ok bool, why string) string {
	if ok {
		return "Approve at the head shown and merge, as " + github.Login
	}
	return why
}

// mergeResult says how an attempt ended, as the MyGo client says it, and
// the badge's variant for it.
func mergeResult(r github.Result) (string, ui.BadgeVariant) {
	text := map[string]string{
		"merged":   "Merged as " + github.Login + ".",
		"queued":   "Added to the merge queue.",
		"approved": "Approved, not merged.",
		"refused":  "Refused.",
		"failed":   "Failed.",
		"unknown":  "Its end is unknown: check it again.",
	}[r.Kind]
	if text == "" {
		text = r.Kind + "."
	}
	if r.Reason != "" {
		text += " " + r.Reason
	}
	switch r.Kind {
	case "merged", "queued":
		return text, ui.BadgeSuccess
	case "approved", "unknown":
		return text, ui.BadgeWarn
	}
	return text, ui.BadgeDanger
}

// methodName is how the gates' read merges it, in words.
func methodName(p github.Preview) string {
	if p.Queue {
		return "the merge queue"
	}
	if p.Method == "" {
		return "GitHub's default method"
	}
	return strings.ToLower(p.Method)
}

func shortHead(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// mergeAll is Merge All Ready (ADR-0031), heading the Inbox's pull
// requests as the MyGo client's mergeAllBar does: the action and why it is
// off, then where the run stands, its confirmation, and how it ended; nil
// when there is nothing to show. state.GitHub holds every rule; the card
// only draws it.
func (c *client) mergeAll() core.Node {
	g := c.gh
	if g == nil {
		return nil
	}
	rows := c.store.Data.PullsToReview
	m := g.MergeAll()
	// Each card says why approve and merge are off, or offers the sign-in.
	if m.Phase == state.MergeAllNone && (len(rows()) == 0 || !g.On() || !g.Status().SignedIn) {
		return nil
	}
	colors := ui.ColorsOf(c.cx)
	out := []core.Node{workspaceui.Row(ui.Icon(lucide.GitMerge, 18, colors.Accent), core.Text{Content: "Merge all ready", Style: ui.TypeLabel})}
	switch m.Phase {
	case state.MergeAllNone:
		ok, why := g.CanMergeAll(rows())
		text, tip := "Approve and merge every pull request whose gates pass now, one at a time, as "+github.Login+".", "Read the gates of every pull request pocketd backs, then confirm the list"
		if !ok {
			text, tip = "Merge all ready is off: "+why+".", why
		}
		start := ui.Button{Text: "Merge all ready", Variant: ui.ButtonSecondary, Disabled: func() bool { return !ok }, OnPress: func() { g.MergeAllReady(rows); c.changed() }}
		out = append(out, c.caption(text), workspaceui.Row(ui.Tooltip{Text: tip, Trigger: start}))
	case state.MergeAllChecking:
		out = append(out, c.caption("Reading the merge gates on GitHub: "+strconv.Itoa(m.Checked)+" of "+strconv.Itoa(m.Checked+m.Checking)+" read…"),
			workspaceui.Row(ui.Tooltip{Text: "Stop the checks: nothing is merged", Trigger: c.button("Stop", g.StopMergeAll)}))
	case state.MergeAllConfirming:
		title, text := mergeAllQuestion(m)
		out = append(out, c.caption("Merge all ready waits for your confirmation of "+pullCount(len(m.Items))+"."),
			workspaceui.Row(ui.Button{Text: "Review and confirm", OnPress: func() {
				c.confirm(title, text, "Approve and merge "+strconv.Itoa(len(m.Items)), func() { g.ConfirmMergeAll(m.ID, c.store.ReadPull) })
			}}, c.button("Drop the list", func() { g.CancelMergeAll(m.ID) })))
	case state.MergeAllMerging:
		text := "Approving and merging " + strconv.Itoa(m.Turn+1) + " of " + strconv.Itoa(len(m.Items)) + " as " + github.Login
		if m.Turn < len(m.Items) {
			text += ": " + m.Items[m.Turn].Ref.String()
		}
		// An earlier merge moved its base: its gates are read again there
		// first (ADR-0038).
		if m.Rechecking {
			text += " · checking it again at its new base"
		}
		if m.Stopping {
			text += " · stopping after this one"
		}
		stop := ui.Button{Text: "Stop after this one", Variant: ui.ButtonSecondary, Disabled: func() bool { return m.Stopping }, OnPress: func() { g.StopMergeAll(); c.changed() }}
		out = append(out, c.caption(text+"…"), workspaceui.Row(ui.Tooltip{Text: "Stop once the merge under way ends", Trigger: stop}))
	case state.MergeAllEnded:
		// How the run's items ended, under the card's heading.
		out = append(out, core.Text{Content: m.Counts().String(), Style: ui.TypeTitle, Selectable: core.SelectableOn})
		if m.End != "" {
			out = append(out, c.caption(strings.ToUpper(m.End[:1])+m.End[1:]+"."))
		}
		// Each that did not merge, with why; those that wait for their
		// dependency bot to rebase them come after, apart, so this list is
		// of failures (the owner's choice of 2026-11-21).
		for _, it := range m.Items {
			if it.Result.Kind == "merged" || it.Rebase != "" {
				continue
			}
			if !it.Tried() {
				out = append(out, workspaceui.Row(ui.Badge{Text: "not tried"}, c.caption(it.Ref.String())))
				continue
			}
			text, variant := mergeResult(it.Result)
			name := it.Ref.String()
			if it.Rechecked {
				name += " (checked again at its new base)"
			}
			out = append(out, workspaceui.Row(ui.Badge{Text: it.Result.Kind, Variant: variant}, c.caption(name+": "+text)))
		}
		for _, it := range m.Items {
			if it.Rebase == "" {
				continue
			}
			// The owner's approval went in before the merge was refused.
			why := it.Result.Reason
			if it.Result.Kind == "approved" {
				why = "approved as " + github.Login + ", not merged: " + why
			}
			out = append(out, workspaceui.Row(ui.Badge{Text: "rebase"}, c.caption(it.Ref.String()+": waits for "+it.Rebase+" to rebase it ("+why+")")))
		}
		out = append(out, workspaceui.Row(c.button("Dismiss", g.DismissMergeAll)))
	}
	return workspaceui.Card{Children: out}
}

// mergeAllQuestion is Merge All Ready's one confirmation: who approves and
// how, then every pull request it lists, by the title GitHub read, at its
// head commit, into its base, with its method, and whether it goes without
// its diff opened, then how many pocketd backs that it leaves out, and
// where each says why.
func mergeAllQuestion(m state.MergeAll) (title, text string) {
	lines := []string{"PocketGo approves each at the head shown as " + github.Login + ", then merges it with the method shown, one at a time. " +
		"Right before each, it reads the pull request from pocketd again and its gates on GitHub again; one that fails stays in the Inbox " +
		"with its reason, and the rest go on. One listed without its diff opened whose base an earlier merge moved is checked again " +
		"at the new base first."}
	for _, it := range m.Items {
		p := it.Merge.Preview
		// The title GitHub read with the head and base shown.
		name := p.Title
		if name == "" {
			name = it.Pull.Title
		}
		line := "at commit " + p.Head + " into " + p.BaseRef + " · " + methodName(p)
		// A dependency bot's risk:high pull request goes without its diff
		// opened (ADR-0031).
		if it.Waived {
			line += " · risk:high, by a dependency bot: no diff opened"
		}
		if it.Note != "" {
			line += " · " + it.Note
		}
		lines = append(lines, strings.Join(nonempty(it.Ref.String(), name), " · ")+"\n"+line)
	}
	if left := m.Checked - len(m.Items); left > 0 {
		lines = append(lines, notListed(left))
	}
	return "Approve and merge " + pullCount(len(m.Items)) + "?", strings.Join(lines, "\n\n")
}

// pullCount is n pull requests, in words.
func pullCount(n int) string {
	if n == 1 {
		return "1 pull request"
	}
	return strconv.Itoa(n) + " pull requests"
}

// notListed says that n pull requests pocketd backs were read and do not
// pass now.
func notListed(n int) string {
	if n == 1 {
		return "1 other pull request pocketd backs does not pass now: its card says why."
	}
	return strconv.Itoa(n) + " other pull requests pocketd backs do not pass now: each card says why."
}
