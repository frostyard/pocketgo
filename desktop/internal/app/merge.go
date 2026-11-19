package app

import (
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// githubOwners are the accounts whose repositories the owner merges in:
// the sign-in checks the App on each (ADR-0018).
var githubOwners = []string{github.Login, "frostyard"}

// mergeRow is a pull request card's approve and merge (ADR-0018): Check
// to Merge reads the gates on GitHub as the owner; the card then says why
// they refuse, or offers Approve + Merge at the head they read, after
// Open Diff for a risk:high one, and a confirmation. state.GitHub holds
// every rule; the row only draws it.
func (a *App) mergeRow(c *ui.Context, key string, p proto.PullRequest) {
	g := a.gh
	if !g.On() {
		if a.opts.GitHubAuth != nil && g.Off() != "" {
			frostui.Caption(c, "Approve and merge are off: "+g.Off()).Wrap()
		}
		return
	}
	ref, ok := state.MergeRef(p)
	if !ok {
		return
	}
	if phase, _, _ := g.SignIn(); phase != state.SignedIn {
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			frostui.Caption(c, "Sign in to GitHub to approve and merge it as "+github.Login+".").Grow(1)
			if ui.Button(c.Key(key+":github-sign-in"), "Sign In to GitHub").Clicked() {
				a.openGitHubSignIn()
			}
		})
		return
	}
	m := g.Merge(ref)
	ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Wrap().Children(func() {
		switch m.Phase {
		case state.MergeNone:
			frostui.Caption(c, "Its merge gates are read on GitHub, as "+github.Login+".").Grow(1)
			if ui.Button(c.Key(key+":check"), "Check to Merge").Clicked() {
				g.Check(ref)
			}
		case state.MergeChecking:
			frostui.Loading(c, "Reading its merge gates on GitHub…").Grow(1)
		case state.MergeActing:
			frostui.Loading(c, "Approving and merging as "+github.Login+"…").Grow(1)
		case state.MergeDone:
			text, tone := mergeResult(m.Result)
			frostui.Pill(c, m.Result.Kind, tone)
			frostui.Caption(c, text).Wrap().Grow(1)
			if ui.Button(c.Key(key+":check"), "Check Again").Clicked() {
				g.Check(ref)
			}
		default:
			a.checkedRow(c, key, ref, m)
		}
	})
}

// checkedRow is the card's row once the gates were read: why they refuse,
// or the merge they allow.
func (a *App) checkedRow(c *ui.Context, key string, ref github.Ref, m state.Merge) {
	g := a.gh
	p := m.Preview
	switch {
	case m.Err != "":
		frostui.Problem(c, "Reading its gates failed: "+m.Err).Wrap().Grow(1)
	case !p.Eligible:
		frostui.Pill(c, "not mergeable", frostui.Warning)
		frostui.Caption(c, p.Reason).Wrap().Grow(1)
	default:
		text := "Gates pass at " + shortHead(p.Head) + " · " + methodName(p)
		if p.Note != "" {
			text += " · " + p.Note
		}
		frostui.Caption(c, text).Wrap().Grow(1)
		if p.RiskHigh {
			if link, ok := state.DiffURL(p); ok && ui.Button(c.Key(key+":diff"), "Open Diff").Tooltip(link).Clicked() {
				c.OpenURLThen(link, func(err error) {
					if err != nil {
						a.toast("Opening the diff failed: " + err.Error())
						return
					}
					g.OpenedDiff(ref)
				})
			}
		}
	}
	if ui.Button(c.Key(key+":check"), "Check Again").Clicked() {
		g.Check(ref)
	}
	ok, why := g.CanMerge(ref)
	if m.Err == "" && p.Eligible {
		if ui.PrimaryButton(c.Key(key+":merge"), "Approve + Merge").Disabled(!ok).Tooltip(mergeTooltip(ok, why)).Clicked() {
			a.confirmMerge(ref, p)
		}
	}
}

func mergeTooltip(ok bool, why string) string {
	if ok {
		return "Approve at the head shown and merge, as " + github.Login
	}
	return why
}

// confirmMerge asks before the approval and the merge, naming the pull
// request, the head they hold for and the method.
func (a *App) confirmMerge(ref github.Ref, p github.Preview) {
	verb := "merges it"
	if p.Queue {
		verb = "adds it to the merge queue"
	}
	a.dialogs.Ask(frostui.Confirm{
		Title: "Approve and merge " + ref.String() + "?",
		Body: "PocketGo approves it at " + shortHead(p.Head) + " as " + github.Login + ", then " + verb + " with " + methodName(p) +
			", if the gates still pass at that head.",
		Action: "Approve + Merge", OnConfirm: func() {
			a.gh.ApproveAndMerge(ref, func(r github.Result) {
				text, _ := mergeResult(r)
				a.toast(ref.String() + ": " + text)
			})
		},
	})
}

// mergeResult says how an attempt ended, in its tone.
func mergeResult(r github.Result) (string, frostui.Tone) {
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
		return text, frostui.Success
	case "approved", "unknown":
		return text, frostui.Warning
	default:
		return text, frostui.Danger
	}
}

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

// openGitHubSignIn opens the sign-in window and starts the device flow.
func (a *App) openGitHubSignIn() {
	if !a.gh.On() {
		return
	}
	a.ghSignIn = true
	if phase, _, _ := a.gh.SignIn(); phase != state.SignInStarting && phase != state.SignInCode {
		a.gh.BeginSignIn()
	}
}

// githubSignInView is the sign-in window: the code to enter at GitHub's
// device page, and where the sign-in stands. It closes once signed in.
func (a *App) githubSignInView(c *ui.Context) {
	if !a.ghSignIn {
		return
	}
	phase, code, fail := a.gh.SignIn()
	if phase == state.SignedIn && fail == "" {
		a.ghSignIn = false
		a.toast("Signed in to GitHub as " + github.Login)
		return
	}
	open := true
	ui.Modal(c, &open, func() {
		ui.Column(c).Width(460).Gap(frostui.SpaceM).AlignItems(ui.Stretch).Role(ui.RoleAlertDialog).Label("Sign in to GitHub").Children(func() {
			ui.Text(c, "Sign in to GitHub").FontSize(18).Bold().Wrap()
			frostui.Muted(c, "PocketGo approves and merges as "+github.Login+" through the GitHub App "+github.AppName+". "+
				"Enter the code at GitHub's device page, and check that GitHub names that App.").Wrap()
			switch phase {
			case state.SignInStarting:
				frostui.Loading(c, "Asking GitHub for a code…")
			case state.SignInCode:
				ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
					frostui.Mono(c, code.UserCode).FontSize(22).Bold().Selectable().Grow(1)
					if ui.Button(c, "Copy Code").Clicked() {
						c.Services().WriteClipboard(code.UserCode)
					}
					if ui.PrimaryButton(c, "Open GitHub").Tooltip(github.DeviceURL).Clicked() {
						c.OpenURLThen(github.DeviceURL, func(err error) {
							if err != nil {
								a.toast("Opening " + github.DeviceURL + " failed: " + err.Error())
							}
						})
					}
				})
				frostui.Loading(c, "Waiting for the code at "+github.DeviceURL+"…")
			case state.SignInChecking:
				frostui.Loading(c, "Checking the new sign-in…")
			case state.SignInSaving:
				frostui.Loading(c, "Keeping the sign-in in the keyring…")
			}
			if fail != "" {
				frostui.Problem(c, fail).Wrap()
			}
			ui.Row(c).Gap(frostui.SpaceS).Justify(ui.End).Children(func() {
				if fail != "" && ui.Button(c, "Try Again").Clicked() {
					a.gh.BeginSignIn()
				}
				if ui.Button(c, "Close").Clicked() {
					open = false
				}
			})
		})
	})
	if !open {
		a.gh.CancelSignIn()
		a.ghSignIn = false
	}
}

// confirmGitHubSignOut asks before the GitHub sign-in goes.
func (a *App) confirmGitHubSignOut() {
	a.dialogs.Ask(frostui.Confirm{
		Title:  "Sign out of GitHub?",
		Body:   "PocketGo forgets the GitHub sign-in in the keyring, and approves and merges nothing until you sign in again.",
		Action: "Sign Out", Destructive: true, OnConfirm: func() {
			a.gh.SignOut(func(err error) {
				if err != nil {
					a.toast("Signing out of GitHub: " + err.Error())
					return
				}
				a.toast("Signed out of GitHub")
			})
		},
	})
}
