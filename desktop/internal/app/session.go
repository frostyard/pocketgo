package app

import (
	"strings"

	"github.com/egoist/mygo/transfer"
	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// transcriptView is the session pane's state: the rows it shows, built
// again only when the open session changed (state.OpenSession.Rev), not
// for other sessions' updates, and what the owner opened.
type transcriptView struct {
	list ui.ListState
	rows []state.Row
	id   int64
	rev  uint64
	// question is the waiting question the rows show (ADR-0048), its id
	// and state: a change of it builds them again.
	question string
	open     map[string]bool // folds the owner opened: thinking and tool calls
	// picks are the options checked on a question's card that takes
	// several, by question.
	picks map[string]map[int]bool
}

// composer is the message field's state: a draft for each session.
type composer struct {
	drafts map[int64]*string
	field  ui.Handle
	// focus asks the field for the keyboard focus on the next frame, as
	// after opening a session.
	focus bool
	// picking says the file chooser is open (pickAttachments).
	picking bool
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
	title := sessionTitle(s, o.ID)
	ui.Column(c).Grow(1).Children(func() {
		ui.Text(c, title).Bold().SingleLine()
		frostui.Caption(c, sessionMeta(s)).SingleLine()
	})
	statePill(c, a.st.Data.StateOf(o.ID))
	a.sessionActions(c, o, s)
}

// sessionTitle is the title of the open session's bar.
func sessionTitle(s proto.Session, id int64) string {
	if s.Title == "" {
		return "Session " + itoa(int(id))
	}
	return s.Title
}

// sessionMeta is the line under a session's title: where it works, its
// model and its thinking.
func sessionMeta(s proto.Session) string {
	meta := []string{s.Cwd, s.Model.String()}
	if s.Thinking != "" {
		meta = append(meta, "thinking "+s.Thinking)
	}
	return strings.Join(meta, " · ")
}

// sessionActions are the open session's buttons: Abort while it runs, and
// a standing session's Start Over.
func (a *App) sessionActions(c *ui.Context, o *state.OpenSession, s proto.Session) {
	if o.Live.Busy {
		if frostui.IconButton(c, "process-stop", "Abort the run").Disabled(a.st.Aborting(o.ID).Busy).Clicked() {
			a.st.Abort(o.ID)
		}
	}
	if s.Standing {
		// A standing session's fresh context, behind a confirmation
		// (ADR-0026), offered only when pocketd would take it: its label
		// says why not.
		label, why := "Start this session over", state.StartOverBlocked(s, o)
		if why != "" {
			label = why
		}
		if frostui.IconButton(c, "edit-clear-all", label).Disabled(why != "" || a.st.StartingOver(o.ID).Busy).Clicked() {
			a.confirmStartOver(s)
		}
	}
}

// confirmStartOver asks before the owner starts a standing session over.
func (a *App) confirmStartOver(s proto.Session) {
	id := s.ID
	a.dialogs.Ask(frostui.Confirm{
		Title: state.StartOverQuestion(s), Body: state.StartOverBody,
		Action: "Start Over", Destructive: true, OnConfirm: func() { a.st.StartOver(id) },
	})
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
	// The session's question that waits shows in its transcript, after
	// its call (ADR-0048).
	var question *proto.Question
	shown := ""
	if q, ok := a.st.Data.Question(o.ID); ok {
		question, shown = &q, q.ID+":"+string(q.State)
	}
	if o.ID != tr.id || o.Rev() != tr.rev || tr.rows == nil || shown != tr.question {
		tr.id, tr.rev, tr.question = o.ID, o.Rev(), shown
		tr.rows = state.BuildRows(o.Entries, o.Live, a.st.Data.ApprovalsFor(o.ID), question)
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
			if o.Err != "" {
				// It loaded before, and loading it again failed: what it
				// shows may miss what happened since.
				ui.Row(c).Gap(frostui.SpaceM).AlignItems(ui.Center).Padding(frostui.SpaceS, a.gutter()).Children(func() {
					frostui.Problem(c, "This transcript may be out of date: loading it again failed: "+o.Err).Wrap().Grow(1)
					if ui.Button(c, "Try Again").Clicked() {
						a.st.Open(o.ID)
					}
				})
			}
			rows := tr.rows
			tr.list.Key = func(i int) any { return rows[i].Key }
			tr.list.FollowEnd = true
			ui.List(c, &tr.list, len(rows), func(i int) {
				ui.Column(c).AlignItems(ui.Center).Children(func() {
					ui.Column(c).MaxWidth(860).FillWidth().AlignItems(ui.Stretch).Children(func() {
						a.rowView(c, &rows[i])
					})
				})
			}).Grow(1).Padding(frostui.SpaceL, a.gutter()).Gap(frostui.SpaceM).Justify(ui.End).Children(func() {
				if len(rows) == 0 {
					frostui.Empty(c, "mail-message-new", "A fresh start", a.composerHints().start)
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
	startOver := a.st.StartingOver(o.ID).Err
	if o.Live.Failure == "" && o.Live.CompactionFailure == "" && startOver == "" && len(approvals) == 0 {
		return
	}
	ui.Column(c).AlignItems(ui.Center).Padding(0, a.gutter(), frostui.SpaceS, a.gutter()).Children(func() {
		ui.Column(c).MaxWidth(860).FillWidth().Gap(frostui.SpaceS).AlignItems(ui.Stretch).Children(func() {
			if o.Live.Failure != "" {
				frostui.Banner(c, frostui.Danger, "dialog-error", "The run failed: "+o.Live.Failure, "", false)
			}
			if o.Live.CompactionFailure != "" {
				frostui.Banner(c, frostui.Warning, "dialog-warning", "Compacting the context failed: "+o.Live.CompactionFailure, "", false)
			}
			if startOver != "" {
				frostui.Banner(c, frostui.Danger, "dialog-error", "Starting over failed: "+startOver, "", false)
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
	case state.RowCompaction:
		// The summary that stands for what came before, folded as a
		// reply's thinking is (ADR-0026).
		a.fold(c, r.Key+":summary", "Compacted the context", strings.TrimLeft(firstLine(r.Text), "# "), func() {
			markdown.View(c.Key(r.Key), r.Text, &a.md)
		})
	case state.RowQuestion:
		a.questionCard(c, *r.Question)
	case state.RowWorking:
		text := r.Text
		if text == "" {
			text = "Working…"
		}
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			ui.Spinner(c).Size(14, 14).Label(strings.TrimSuffix(text, "…"))
			frostui.Muted(c, text)
		})
	}
}

// userRow draws a message to the agent: the owner's, or a supervisor's,
// with the supervisor's name and a supervisor pill (ADR-0032); then its
// words, a chip for each file it attached, its path in the tooltip, and
// the count of the images it gave the model, as frostyard/pocket shows
// them (ADR-0029).
func (a *App) userRow(c *ui.Context, r *state.Row) {
	t := c.Theme()
	name, supervisor := "You", false
	if session, ok := r.FromSupervisor(); ok {
		name, supervisor = a.senderName(session), true
	}
	ui.Row(c).Gap(frostui.SpaceM).AlignItems(ui.Start).Padding(frostui.SpaceM).Radius(10).
		Background(frostui.Panel(t)).BorderWidth(1, 1, 1, 1).BorderColor(t.Border).Children(func() {
		frostui.SquareAvatar(c, name, 28)
		ui.Column(c).Grow(1).Gap(4).AlignItems(ui.Stretch).Children(func() {
			ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
				frostui.Strong(c, name)
				if supervisor {
					frostui.Pill(c, "supervisor", frostui.Accent)
				}
				if r.Queued {
					frostui.Pill(c, "queued", frostui.Neutral)
				}
			})
			// A message can carry files alone, with no words of its own.
			if strings.TrimSpace(r.Text) != "" {
				markdown.View(c.Key(r.Key), r.Text, &a.md)
			}
			if len(r.Files) > 0 || r.Images > 0 {
				ui.Row(c).Gap(frostui.SpaceXS).Wrap().Children(func() {
					for _, f := range r.Files {
						frostui.IconPill(c.Key("file:"+f.Path), "mail-attachment", f.Name, frostui.Neutral).Tooltip(f.Path)
					}
					if r.Images > 0 {
						frostui.IconPill(c.Key("images"), "insert-image", plural(r.Images, "image", "images"), frostui.Neutral)
					}
				})
			}
		})
	})
}

// senderName names the supervisor's session that sent a message: its
// profile's name, else its title, else Supervisor.
func (a *App) senderName(id int64) string {
	if s, ok := a.st.Data.Session(id); ok {
		if s.Profile != "" {
			return a.agentName(s.Profile)
		}
		if s.Title != "" {
			return s.Title
		}
	}
	return "Supervisor"
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

// composerHints are what the composer says before a message is typed:
// start in an empty transcript, idle and busy in the field, while no run
// works and while one does.
type composerHints struct{ start, idle, busy string }

// composerHints are the window's: a desktop's name its keys; a compact
// window's are short, with no keys, which a phone's keyboard does not
// show.
func (a *App) composerHints() composerHints {
	if a.compact {
		return composerHints{start: "Tell the agent what to do.", idle: "Message the agent",
			busy: "The agent is working. Your message still goes to it."}
	}
	return composerHints{start: "Tell the agent what to do. Enter sends, Shift+Enter starts a new line.",
		idle: "Message the agent. Enter sends, Shift+Enter starts a new line.",
		busy: "The agent is working. Enter still sends your message to the session."}
}

// composerView is the message field and its buttons.
func (a *App) composerView(c *ui.Context, o *state.OpenSession) {
	t := c.Theme()
	cp := &a.composer
	id := o.ID
	draft := cp.draft(id)
	sending := a.st.Sending(id)
	readOnly, why := a.st.Data.ReadOnly(id)
	ui.Column(c).AlignItems(ui.Center).Padding(0, a.gutter(), frostui.SpaceL, a.gutter()).Children(func() {
		ui.Column(c).MaxWidth(860).FillWidth().AlignItems(ui.Stretch).Gap(frostui.SpaceXS).Children(func() {
			if readOnly {
				// No message goes to it: the owner reads it, and may stop it.
				ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
					frostui.Caption(c, why).Wrap().Grow(1)
					if o.Live.Busy {
						if ui.Button(c, "Abort").Disabled(a.st.Aborting(id).Busy).Clicked() {
							a.st.Abort(id)
						}
					}
				})
				return
			}
			if sending.Err != "" {
				frostui.Problem(c, capitalize(sending.Err)+".").Wrap()
			}
			// While the session's question waits, the field answers it
			// (the owner's decision, ADR-0048), from the question's own
			// draft, so its words never go elsewhere (Codex review of #95,
			// round 1).
			composing := a.st.Data.ComposerOf(id)
			var fieldKey any = id
			if composing.Answers() {
				draft, fieldKey = a.st.QuestionDraft(*composing.Question), "answer:"+composing.Question.ID
			}
			if a.st.CheckingAnswer(id) {
				// A lost reply is checked before its words count as refused
				// (Codex review of #95, round 2).
				frostui.Loading(c, "Checking whether pocketd took your answer…")
			}
			// Each question's refused words, until the owner dismisses them
			// (Codex review of #95, round 2).
			for _, refused := range a.st.QuestionRefusals(id) {
				key := "question-refusal:" + refused.Question
				ui.Column(c.Key(key)).Gap(frostui.SpaceXS).AlignItems(ui.Stretch).Children(func() {
					ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
						frostui.Problem(c, "Your answer was not taken: "+strings.TrimSuffix(refused.Why, ".")+".").Wrap().Grow(1)
						if ui.Button(c.Key(key+":dismiss"), "Dismiss").Clicked() {
							a.st.DismissQuestionRefusal(refused.Question)
						}
					})
					if strings.TrimSpace(refused.Answer) != "" {
						frostui.Caption(c, "Your words:")
						ui.Text(c, refused.Answer).Wrap().Selectable()
					}
				})
			}
			if changing := a.st.ChangingModel(id); changing.Err != "" {
				frostui.Problem(c, "Changing the model failed: "+changing.Err).Wrap()
			}
			ui.Column(c).AlignItems(ui.Stretch).Radius(12).BorderWidth(1, 1, 1, 1).BorderColor(t.Border).
				Background(frostui.Panel(t)).Padding(frostui.SpaceS, frostui.SpaceM).Gap(frostui.SpaceXS).Children(func() {
				a.attachmentChips(c, id)
				hints := a.composerHints()
				placeholder := hints.idle
				if o.Live.Busy {
					placeholder = hints.busy
				}
				answering := state.Pending{}
				if composing.Answers() {
					placeholder = composing.Hint(a.compact)
					answering = a.st.AnsweringQuestion(composing.Question.ID)
				}
				svc := c.Services()
				label := "Message"
				if composing.Answers() {
					label = "Answer"
				}
				input := ui.TextAreaBase(c.Key(fieldKey), draft).Lines(1, 10).Placeholder(placeholder).Label(label).
					Bind(&cp.field).Disabled(sending.Busy || answering.Busy).
					OnTextKey(func(k ui.TextKey) bool {
						if k.Key == ui.KeyEnter && k.Mods == 0 {
							a.submit(id, composing, k.Text())
							return true
						}
						return false
					}).
					OnPaste(func(string) bool {
						// An answer carries text alone: a pasted file is not
						// attached to it (state.Composer.Attaches).
						if !composing.Attaches() {
							return false
						}
						return a.pasteAttachments(id, svc.ReadClipboardData(transfer.FileList, transfer.PNG))
					})
				if paths := input.DroppedFiles(); len(paths) > 0 && composing.Attaches() {
					a.attachFiles(id, paths)
				}
				// The window's layout of this frame decides: a session the
				// start opens again comes before the first frame measured
				// the window. A phone's keyboard would come up over half
				// the transcript (Codex review of #85, round 3).
				if cp.focus {
					if !a.compact {
						input.AutoFocus()
					}
					cp.focus = false
				}
				// A phone's width holds the model's chip or the buttons, not
				// both: the chip takes a row of its own there, and Attach
				// files its icon alone (Codex review of #85, round 2).
				if a.compact {
					ui.Row(c).AlignItems(ui.Center).Children(func() { a.modelChip(c, id) })
				}
				ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
					if a.opts.PickFiles != nil {
						var attach ui.Element
						if a.compact {
							attach = frostui.IconButton(c, "mail-attachment", "Attach files")
						} else {
							attach = ui.Button(c, "Attach files")
						}
						if attach.Disabled(cp.picking || !composing.Attaches()).Clicked() {
							a.pickAttachments(id)
						}
					}
					if !a.compact {
						a.modelChip(c, id)
					}
					if sending.Busy || answering.Busy {
						ui.Spinner(c).Size(14, 14).Label("Sending")
						frostui.Caption(c, "Sending…")
					}
					ui.Spacer(c)
					if o.Live.Busy {
						if ui.Button(c, "Abort").Disabled(a.st.Aborting(id).Busy).Clicked() {
							a.st.Abort(id)
						}
					}
					if composing.Answers() {
						// The words are the answer: files make none.
						if frostui.LabeledButton(c, "document-send", "Send Answer", true).Disabled(strings.TrimSpace(*draft) == "" || answering.Busy).Clicked() {
							a.submit(id, composing, *draft)
						}
						return
					}
					// Files alone make a message (ADR-0029).
					empty := strings.TrimSpace(*draft) == "" && len(a.st.Attachments(id)) == 0
					if frostui.LabeledButton(c, "document-send", "Send", true).Disabled(empty || !a.st.MaySend(id)).Clicked() {
						a.submit(id, composing, *draft)
					}
				})
			})
		})
	})
}

// submit sends text from session id's composer where its draft goes
// (composing, as the frame that drew the field found it): the answer to
// the question it was written for, whose draft the store clears once
// pocketd took it and keeps when the question settled meanwhile
// (state.Store.QuestionDraft); or a message with the attached files, whose
// draft clears when pocketd took it.
func (a *App) submit(id int64, composing state.Composer, text string) {
	if composing.Answers() {
		if strings.TrimSpace(text) == "" || a.st.AnsweringQuestion(composing.Question.ID).Busy {
			return
		}
		a.st.AnswerQuestion(*composing.Question, text, nil, func(error) {})
		return
	}
	if (strings.TrimSpace(text) == "" && len(a.st.Attachments(id)) == 0) || !a.st.MaySend(id) {
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

// questionCard is a session's question that waits for the owner, inline
// in its transcript (ADR-0048): the question, the options the agent
// suggests, each a button that answers at once, or, when the owner may
// pick several, checkboxes and Send; and Skip. The message box below
// takes any other answer: the card has no field of its own. It says when
// the question is skipped if no answer comes.
func (a *App) questionCard(c *ui.Context, q proto.Question) {
	t := c.Theme()
	key := "question:" + q.ID
	pending := a.st.AnsweringQuestion(q.ID)
	ui.Column(c.Key(key)).Gap(frostui.SpaceS).Padding(frostui.SpaceM, frostui.SpaceL).AlignItems(ui.Stretch).
		Radius(10).Background(frostui.Panel(t)).BorderWidth(1, 1, 1, 1).BorderColor(frostui.ToneColor(t, frostui.Accent)).
		Role(ui.RoleGroup).Label("Question for you").Children(func() {
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			frostui.Icon(c, "help-browser").TextColor(frostui.ToneColor(t, frostui.Accent))
			a.headTitle(c, a.questionFrom(q.SessionID)+" waits for your answer")
		})
		ui.Text(c, q.Question).Wrap().Selectable()
		switch {
		case len(q.Options) > 0 && q.Multiple:
			if a.tr.picks == nil {
				a.tr.picks = map[string]map[int]bool{}
			}
			picks := a.tr.picks[q.ID]
			if picks == nil {
				picks = map[int]bool{}
				a.tr.picks[q.ID] = picks
			}
			ui.Column(c).Gap(frostui.SpaceXS).AlignItems(ui.Start).Children(func() {
				// Keyed by place: options may repeat.
				for i, option := range q.Options {
					checked := picks[i]
					if ui.Checkbox(c.Key(key+":pick:"+itoa(i)), &checked, option).Disabled(pending.Busy).Changed() {
						picks[i] = checked
					}
				}
			})
			var picked []int
			for i := range q.Options {
				if picks[i] {
					picked = append(picked, i)
				}
			}
			ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
				if ui.PrimaryButton(c.Key(key+":send"), "Send Choices").Disabled(pending.Busy || len(picked) == 0).Clicked() {
					a.st.AnswerQuestion(q, "", picked, func(error) {})
				}
			})
		case len(q.Options) > 0:
			ui.Row(c).Gap(frostui.SpaceS).Wrap().Children(func() {
				for i, option := range q.Options {
					if ui.Button(c.Key(key+":option:"+itoa(i)), option).Disabled(pending.Busy).Tooltip("Answer “" + option + "”").Clicked() {
						a.st.AnswerQuestion(q, "", []int{i}, func(error) {})
					}
				}
			})
		}
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			// With the agent's hint for a free answer (Codex review of #95,
			// round 1).
			frostui.Caption(c, state.QuestionCue(q, a.opts.Now())).Wrap().Grow(1)
			if ui.Button(c.Key(key+":skip"), "Skip").Disabled(pending.Busy).Tooltip("Go on without an answer").Clicked() {
				a.st.SkipQuestion(q)
			}
		})
		if pending.Busy {
			frostui.Loading(c, "Sending your answer…")
		}
		if pending.Err != "" {
			frostui.Problem(c, capitalize(strings.TrimSuffix(pending.Err, "."))+".").Wrap()
		}
	})
}

// questionFrom names the agent of the session a question comes from: its
// profile's name, else the session's title, else The agent.
func (a *App) questionFrom(id int64) string {
	if s, ok := a.st.Data.Session(id); ok {
		if s.Profile != "" {
			return a.agentName(s.Profile)
		}
		if s.Title != "" {
			return s.Title
		}
	}
	return "The agent"
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func truncate(s string, n int) string { return state.Truncate(s, n, false) }

// modelChip shows the session's model and its thinking level, and changes
// them: frostyard/pocket's chip, whole (ADR-0028). Its menu has a submenu
// per provider with its models, the current one checked, and a Thinking
// submenu when the model takes more than one level. Its rules are
// state.ChipOf's.
func (a *App) modelChip(c *ui.Context, id int64) {
	session, ok := a.st.Data.Session(id)
	if !ok {
		return
	}
	models, problem := a.st.Models()
	chip := state.ChipOf(session, models, problem)
	ui.MenuButton(c, chip.Label, func(m *ui.Menu) {
		if chip.Reload {
			item := m.Item("Load the models again").ID("reload")
			if chip.Problem != "" {
				item.Subtitle("The models did not load: " + chip.Problem)
			}
			if item.Chosen() {
				a.st.LoadModels()
			}
		}
		for _, group := range chip.Providers {
			m.Submenu(group.Provider, func(sm *ui.Menu) {
				for _, item := range group.Models {
					if sm.Item(item.Label).ID(item.ID).Checked(item.Current).Chosen() {
						ref := item.Ref
						a.st.ChangeModel(id, &ref, "")
					}
				}
			})
		}
		if len(chip.Levels) > 0 {
			m.Separator()
			m.Submenu("Thinking", func(sm *ui.Menu) {
				for _, level := range chip.Levels {
					if sm.Item(level.Level).ID(level.ID).Checked(level.Current).Chosen() {
						a.st.ChangeModel(id, nil, level.Level)
					}
				}
			})
		}
	}).Label("Model").Tooltip("Change the model").Disabled(a.st.ChangingModel(id).Busy)
}
