package app

import (
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
)

// transcriptView is the session pane's state: the rows it shows, built
// again only when the data changed (state.Data.Rev), and what the owner
// opened.
type transcriptView struct {
	list ui.ListState
	rows []state.Row
	rev  uint64
	open map[string]bool // folds the owner opened: thinking and tool calls
}

// composer is the message field's state: a draft for each session.
type composer struct {
	drafts map[int64]*string
	field  ui.Handle
	// focus asks the field for the keyboard focus on the next frame, as
	// after opening a session.
	focus bool
}

func (cp *composer) draft(id int64) *string {
	d := cp.drafts[id]
	if d == nil {
		d = new(string)
		cp.drafts[id] = d
	}
	return d
}

// sessionBar is the top bar over the open session: its title, where it
// works, its state, and Abort while it runs.
func (a *App) sessionBar(c *ui.Context) {
	o := a.st.Data.Open()
	s, _ := a.st.Data.Session(o.ID)
	title := s.Title
	if title == "" {
		title = "Session " + itoa(int(o.ID))
	}
	ui.Column(c).Grow(1).Children(func() {
		ui.Text(c, title).Bold().SingleLine()
		meta := []string{s.Cwd, s.Model.String()}
		if s.Thinking != "" {
			meta = append(meta, "thinking "+s.Thinking)
		}
		frostui.Caption(c, strings.Join(meta, " · ")).SingleLine()
	})
	statePill(c, a.st.Data.StateOf(o.ID))
	if o.Live.Busy {
		if frostui.IconButton(c, "process-stop", "Abort the run").Disabled(a.st.Aborting(o.ID).Busy).Clicked() {
			a.st.Abort(o.ID)
		}
	}
}

// sessionPane is the open session: its transcript, what failed, the
// approvals it waits for and the composer.
func (a *App) sessionPane(c *ui.Context) {
	t := c.Theme()
	o := a.st.Data.Open()
	tr := &a.tr
	if tr.open == nil {
		tr.open = map[string]bool{}
	}
	if rev := a.st.Data.Rev(); rev != tr.rev || tr.rows == nil {
		tr.rev = rev
		tr.rows = state.BuildRows(o.Entries, o.Live, a.st.Data.ApprovalsFor(o.ID))
	}
	ui.Column(c).Grow(1).AlignItems(ui.Stretch).Background(t.Background).Children(func() {
		switch {
		case o.Err != "" && !o.Loaded:
			ui.Column(c).Grow(1).Center().Gap(frostui.SpaceM).Children(func() {
				frostui.Problem(c, "Loading the session failed: "+o.Err).Wrap()
				if ui.Button(c, "Try Again").Clicked() {
					a.st.Open(o.ID)
				}
			})
		case !o.Loaded && len(tr.rows) == 0:
			ui.Column(c).Grow(1).Center().Children(func() { frostui.Loading(c, "Opening the session…") })
		default:
			rows := tr.rows
			tr.list.Key = func(i int) any { return rows[i].Key }
			tr.list.FollowEnd = true
			ui.List(c, &tr.list, len(rows), func(i int) {
				ui.Column(c).AlignItems(ui.Center).Children(func() {
					ui.Column(c).MaxWidth(860).FillWidth().AlignItems(ui.Stretch).Children(func() {
						a.rowView(c, &rows[i])
					})
				})
			}).Grow(1).Padding(frostui.SpaceL, frostui.SpaceXL).Gap(frostui.SpaceM).Justify(ui.End).Children(func() {
				if len(rows) == 0 {
					frostui.Empty(c, "mail-message-new", "A fresh start", "Tell the agent what to do. Enter sends, Shift+Enter starts a new line.")
				}
			})
		}
		a.dock(c, o)
		a.composerView(c, o)
	})
}

// dock is what sits above the composer: the run's failure and the
// approvals the session waits for.
func (a *App) dock(c *ui.Context, o *state.OpenSession) {
	approvals := a.st.Data.ApprovalsFor(o.ID)
	if o.Live.Failure == "" && len(approvals) == 0 {
		return
	}
	ui.Column(c).AlignItems(ui.Center).Padding(0, frostui.SpaceXL, frostui.SpaceS, frostui.SpaceXL).Children(func() {
		ui.Column(c).MaxWidth(860).FillWidth().Gap(frostui.SpaceS).AlignItems(ui.Stretch).Children(func() {
			if o.Live.Failure != "" {
				frostui.Banner(c, frostui.Danger, "dialog-error", "The run failed: "+o.Live.Failure, "", false)
			}
			for _, ap := range approvals {
				a.approvalCard(c, ap, false)
			}
		})
	})
}

// rowView draws one row of the transcript.
func (a *App) rowView(c *ui.Context, r *state.Row) {
	switch r.Kind {
	case state.RowUser:
		a.userRow(c, r)
	case state.RowAssistant:
		ui.Column(c).Gap(frostui.SpaceS).AlignItems(ui.Stretch).Children(func() {
			if strings.TrimSpace(r.Thinking) != "" {
				a.fold(c, r.Key+":thinking", "Thinking", firstLine(r.Thinking), func() {
					frostui.Muted(c, r.Thinking).Wrap().Selectable()
				})
			}
			if strings.TrimSpace(r.Text) != "" {
				if r.Live {
					markdown.Stream(c.Key(r.Key), r.Text, &a.md)
				} else {
					markdown.View(c.Key(r.Key), r.Text, &a.md)
				}
			}
		})
	case state.RowTool:
		a.toolRow(c, r)
	case state.RowError:
		frostui.Problem(c, r.Text).Wrap()
	case state.RowNote:
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			ui.Divider(c).Grow(1)
			frostui.Caption(c, r.Text)
			ui.Divider(c).Grow(1)
		})
	case state.RowWorking:
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			ui.Spinner(c).Size(14, 14).Label("Working")
			frostui.Muted(c, "Working…")
		})
	}
}

func (a *App) userRow(c *ui.Context, r *state.Row) {
	t := c.Theme()
	ui.Row(c).Gap(frostui.SpaceM).AlignItems(ui.Start).Padding(frostui.SpaceM).Radius(10).
		Background(frostui.Panel(t)).BorderWidth(1, 1, 1, 1).BorderColor(t.Border).Children(func() {
		frostui.SquareAvatar(c, "You", 28)
		ui.Column(c).Grow(1).Gap(4).AlignItems(ui.Stretch).Children(func() {
			frostui.Strong(c, "You")
			markdown.View(c.Key(r.Key), r.Text, &a.md)
		})
	})
}

// fold is a quiet line that opens to show body.
func (a *App) fold(c *ui.Context, key, title, aside string, body func()) {
	t := c.Theme()
	open := a.tr.open[key]
	ui.Column(c.Key(key)).AlignItems(ui.Stretch).Radius(8).BorderWidth(1, 1, 1, 1).BorderColor(t.Border).Children(func() {
		head := ui.ButtonBase(c).Row().Gap(frostui.SpaceS).AlignItems(ui.Center).Padding(6, frostui.SpaceM).
			Cursor(ui.CursorPointer).Label(title).Expanded(open)
		if head.Hovered() {
			head.Background(t.SurfaceHover)
		}
		if head.Clicked() {
			a.tr.open[key] = !open
			a.tr.list.HoldPlace()
		}
		head.Children(func() {
			frostui.Icon(c, chevron(open))
			frostui.Strong(c, title)
			frostui.Muted(c, truncate(aside, 120)).SingleLine().Grow(1)
		})
		if open {
			ui.Column(c).Padding(frostui.SpaceXS, frostui.SpaceM, frostui.SpaceM, frostui.SpaceM).AlignItems(ui.Stretch).Children(body)
		}
	})
}

// toolRow is a tool call folded to one line: its name, what it works on and
// where it stands; open, its arguments and its result.
func (a *App) toolRow(c *ui.Context, r *state.Row) {
	t := c.Theme()
	tool := r.Tool
	key := r.Key
	open := a.tr.open[key]
	ui.Column(c.Key(key)).AlignItems(ui.Stretch).Radius(8).BorderWidth(1, 1, 1, 1).BorderColor(t.Border).Children(func() {
		head := ui.ButtonBase(c).Row().Gap(frostui.SpaceS).AlignItems(ui.Center).Padding(6, frostui.SpaceM).
			Cursor(ui.CursorPointer).Label(tool.Name + " " + tool.Summary).Expanded(open)
		if head.Hovered() {
			head.Background(t.SurfaceHover)
		}
		if head.Clicked() {
			a.tr.open[key] = !open
			a.tr.list.HoldPlace()
		}
		head.Children(func() {
			frostui.Icon(c, chevron(open))
			frostui.Icon(c, toolIcon(tool.Name))
			frostui.Mono(c, tool.Name).SingleLine()
			frostui.Muted(c, tool.Summary).SingleLine().Grow(1)
			toolStatus(c, tool.State)
		})
		if !open {
			return
		}
		ui.Column(c).Gap(frostui.SpaceS).Padding(frostui.SpaceXS, frostui.SpaceM, frostui.SpaceM, frostui.SpaceM).AlignItems(ui.Stretch).Children(func() {
			if tool.Arguments != "" {
				frostui.Caption(c, "Arguments")
				codeBox(c, tool.Arguments, 200)
			}
			if tool.Result != "" {
				frostui.Caption(c, "Result")
				codeBox(c, tool.Result, 320)
			}
		})
	})
}

// toolStatus is where a tool call stands, in its tone.
func toolStatus(c *ui.Context, s state.ToolState) {
	switch s {
	case state.ToolRunning:
		ui.Spinner(c).Size(14, 14).Label("running")
		frostui.Caption(c, s.String())
	case state.ToolQueued:
		frostui.Pill(c, s.String(), frostui.Neutral)
	case state.ToolWaiting:
		frostui.Pill(c, s.String(), frostui.Warning)
	case state.ToolDone:
		frostui.Pill(c, s.String(), frostui.Success)
	case state.ToolFailed:
		frostui.Pill(c, s.String(), frostui.Danger)
	default:
		frostui.Pill(c, s.String(), frostui.Neutral)
	}
}

func toolIcon(name string) string {
	switch name {
	case "bash":
		return "utilities-terminal"
	case "read", "ls":
		return "text-x-generic"
	case "write", "edit":
		return "document-edit"
	case "grep", "find":
		return "edit-find"
	}
	return "emblem-system"
}

// codeBox shows text in monospace, scrolled within max DIPs.
func codeBox(c *ui.Context, text string, max float32) {
	t := c.Theme()
	text = strings.TrimRight(text, "\n")
	if len(text) > 20000 {
		text = text[:10000] + "\n…\n" + text[len(text)-10000:]
	}
	ui.ScrollBoth(c).MaxHeight(max).Radius(6).Background(frostui.ColorsOf(t).Code).Children(func() {
		frostui.Mono(c, text).Selectable().Padding(frostui.SpaceS, frostui.SpaceM).NoWrap()
	})
}

// composerView is the message field and its buttons.
func (a *App) composerView(c *ui.Context, o *state.OpenSession) {
	t := c.Theme()
	cp := &a.composer
	id := o.ID
	draft := cp.draft(id)
	sending := a.st.Sending(id)
	ui.Column(c).AlignItems(ui.Center).Padding(0, frostui.SpaceXL, frostui.SpaceL, frostui.SpaceXL).Children(func() {
		ui.Column(c).MaxWidth(860).FillWidth().AlignItems(ui.Stretch).Gap(frostui.SpaceXS).Children(func() {
			if sending.Err != "" {
				frostui.Problem(c, capitalize(sending.Err)+".").Wrap()
			}
			ui.Column(c).AlignItems(ui.Stretch).Radius(12).BorderWidth(1, 1, 1, 1).BorderColor(t.Border).
				Background(frostui.Panel(t)).Padding(frostui.SpaceS, frostui.SpaceM).Gap(frostui.SpaceXS).Children(func() {
				placeholder := "Message the agent. Enter sends, Shift+Enter starts a new line."
				if o.Live.Busy {
					placeholder = "The agent is working. Enter still sends your message to the session."
				}
				input := ui.TextAreaBase(c.Key(id), draft).Lines(1, 10).Placeholder(placeholder).Label("Message").
					Bind(&cp.field).Disabled(sending.Busy).
					OnTextKey(func(k ui.TextKey) bool {
						if k.Key == ui.KeyEnter && k.Mods == 0 {
							a.send(id, k.Text())
							return true
						}
						return false
					})
				if cp.focus {
					input.AutoFocus()
					cp.focus = false
				}
				ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
					if sending.Busy {
						ui.Spinner(c).Size(14, 14).Label("Sending")
						frostui.Caption(c, "Sending…")
					}
					ui.Spacer(c)
					if o.Live.Busy {
						if ui.Button(c, "Abort").Disabled(a.st.Aborting(id).Busy).Clicked() {
							a.st.Abort(id)
						}
					}
					empty := strings.TrimSpace(*draft) == ""
					if frostui.LabeledButton(c, "document-send", "Send", true).Disabled(empty || sending.Busy).Clicked() {
						a.send(id, *draft)
					}
				})
			})
		})
	})
}

// send sends text to session id; the draft clears when pocketd took it.
func (a *App) send(id int64, text string) {
	if strings.TrimSpace(text) == "" || a.st.Sending(id).Busy {
		return
	}
	a.st.Send(id, text, func(err error) {
		if err != nil {
			return
		}
		if d := a.composer.drafts[id]; d != nil && *d == text {
			*d = ""
		}
	})
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func truncate(s string, n int) string { return state.Truncate(s, n, false) }
