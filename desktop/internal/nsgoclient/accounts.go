package nsgoclient

import (
	"fmt"
	"strings"

	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/desktop/workspaceui"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
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
	if mode, _ := c.cx.Storage().GetItem("appearance"); mode == "Dark" {
		c.cx.SetAppearance(core.AppearanceDark)
	} else if mode == "Light" {
		c.cx.SetAppearance(core.AppearanceLight)
	}
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
				if err == nil && c.prefs.LastSession != 0 {
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
				c.result(err)
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
				c.cx.Post(func() { c.result(err) })
			}
		})
	}
}
func (c *client) initGitHub(auth state.GitHubAuth) {
	c.gh = state.NewGitHub(state.GitHubOptions{Post: func(fn func()) { c.cx.Post(func() { c.cx.Batch(func() { fn(); c.changed() }) }) }, Auth: auth, NewClient: func(t string) state.GitHubAPI { return github.New(t) }, Owners: []string{github.Login, "frostyard"}})
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
			c.confirm("Sign out of GitHub?", "Remove the owner credential from the desktop keyring.", "Sign out", func() { g.SignOut(c.result) })
		}))
	}
	if g.CanSaveAgain() {
		out = append(out, c.button("Save credential again", func() { g.SaveAgain(c.result) }))
	}
	return workspaceui.Stack(out...)
}
func (c *client) pullMergeActions(p proto.PullRequest, ref github.Ref) core.Node {
	g := c.gh
	m := g.Merge(ref)
	pre := m.Preview
	actions := []core.Node{c.button("Check merge gates", func() { g.Check(p) })}
	var notes []core.Node
	if note := strings.Join(nonempty(m.Err, pre.Reason, pre.Note, m.Result.Kind, m.Result.Reason), " · "); note != "" {
		notes = append(notes, body(note))
	}
	if m.Phase == state.MergeChecked {
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
		allowed, why := g.CanMerge(p)
		if why != "" {
			notes = append(notes, body(why))
		}
		actions = append(actions, ui.Button{Text: "Approve and merge", Disabled: func() bool { return !allowed }, OnPress: func() {
			action := "merge"
			if pre.Queue {
				action = "enqueue"
			}
			c.confirm("Approve "+ref.String()+"?", fmt.Sprintf("Approve as %s and %s commit %s into %s using %s.", github.Login, action, pre.Head, pre.BaseRef, pre.Method), "Approve and merge", func() {
				c.store.ReadPull(p.ID, func(f state.FreshPull, found bool, err error) {
					if err != nil {
						c.result(err)
						return
					}
					if !found {
						c.showNotice(ui.ToastWarning, "Pull request unavailable", "This pull request is no longer listed.")
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
							title, kind = "Pull request approved", ui.ToastSuccess
						case "refused", "failed":
							title, kind = "Couldn’t merge pull request", ui.ToastError
						}
						c.showNotice(kind, title, r.Reason)
						c.changed()
					})
				})
			})
		}})
	}
	return workspaceui.Stack(append(notes, workspaceui.Row(actions...))...)
}
func (c *client) mergeAll() core.Node {
	g := c.gh
	if g == nil {
		return core.View{}
	}
	rows := c.store.Data.PullsToReview
	m := g.MergeAll()
	out := []core.Node{}
	switch m.Phase {
	case state.MergeAllNone:
		if ok, _ := g.CanMergeAll(rows()); ok {
			out = append(out, c.button("Check all ready pull requests", func() { g.MergeAllReady(rows) }))
		}
	case state.MergeAllChecking:
		out = append(out, body("Checking merge gates…"), c.button("Cancel checks", func() { g.CancelMergeAll(m.ID) }))
	case state.MergeAllConfirming:
		lines := []string{}
		for _, i := range m.Items {
			p := i.Merge.Preview
			lines = append(lines, fmt.Sprintf("%s · %s → %s · %s", i.Ref.String(), p.Head, p.BaseRef, p.Method))
		}
		summary := strings.Join(lines, "\n")
		out = append(out, body(summary), c.button("Review batch merge", func() {
			c.confirm("Approve and merge these pull requests?", summary, "Approve batch", func() { g.ConfirmMergeAll(m.ID, c.store.ReadPull) })
		}), c.button("Cancel batch", func() { g.CancelMergeAll(m.ID) }))
	case state.MergeAllMerging:
		out = append(out, body("Merging approved batch…"), c.button("Stop after current merge", g.StopMergeAll))
	case state.MergeAllEnded:
		out = append(out, body(m.Counts().String()+" "+m.End))
		for _, i := range m.Items {
			out = append(out, body(i.Ref.String()+" · "+i.Result.Kind+" "+i.Result.Reason))
		}
		out = append(out, c.button("Dismiss batch results", g.DismissMergeAll))
	}
	return workspaceui.Stack(out...)
}
