package app

import (
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// inboxPane lists what waits for the owner: every open ask, every lesson
// that waits, then every pending approval, oldest first (state.Data.Asks,
// state.Data.Lessons, state.Data.Approvals). It says that nothing waits
// only when every list's last fetch succeeded.
func (a *App) inboxPane(c *ui.Context) {
	t := c.Theme()
	approvals := a.st.Data.Approvals()
	status, problem := a.st.Data.ApprovalsStatus()
	asks := a.st.Data.Asks()
	asksStatus, asksProblem := a.st.Data.AsksStatus()
	lessons := a.st.Data.Lessons()
	lessonsStatus, lessonsProblem := a.st.Data.LessonsStatus()
	// A draft lives only while its question waits.
	open := make(map[int64]bool, len(asks))
	for _, ask := range asks {
		open[ask.ID] = true
	}
	for id := range a.answers {
		if !open[id] {
			delete(a.answers, id)
		}
	}
	ui.Scroll(c).Grow(1).Fill().Background(t.Background).Children(func() {
		ui.Column(c).AlignItems(ui.Center).Padding(frostui.SpaceL, frostui.SpaceXL).Children(func() {
			ui.Column(c).MaxWidth(860).FillWidth().Gap(frostui.SpaceM).AlignItems(ui.Stretch).Children(func() {
				if asksStatus == state.ListFailed {
					a.listProblem(c, "Loading the questions failed: "+asksProblem+". Questions may wait for you that this list does not show.")
				}
				if lessonsStatus == state.ListFailed {
					a.listProblem(c, "Loading the lessons failed: "+lessonsProblem+". Lessons may wait for you that this list does not show.")
				}
				if status == state.ListFailed {
					a.listProblem(c, "Loading the approvals failed: "+problem+". Calls may wait for you that this list does not show.")
				}
				none := len(approvals) == 0 && len(asks) == 0 && len(lessons) == 0
				switch {
				case (status == state.ListLoading || asksStatus == state.ListLoading || lessonsStatus == state.ListLoading) && none:
					frostui.Loading(c, "Loading what waits for you…")
				case status == state.ListLoaded && asksStatus == state.ListLoaded && lessonsStatus == state.ListLoaded && none:
					frostui.Empty(c, "object-select", "Nothing waits for you",
						"Questions from the team's threads, lessons the agents propose, and tool calls that need your approval, show here.")
				}
				for _, ask := range asks {
					a.askCard(c, ask)
				}
				for _, lesson := range lessons {
					a.lessonCard(c, lesson)
				}
				for _, ap := range approvals {
					a.approvalCard(c, ap, true)
				}
			})
		})
	})
}

// listProblem says a list of the Inbox failed to load, with Try Again.
func (a *App) listProblem(c *ui.Context, text string) {
	ui.Row(c).Gap(frostui.SpaceM).AlignItems(ui.Center).Children(func() {
		frostui.Problem(c, text).Wrap().Grow(1)
		if ui.Button(c, "Try Again").Clicked() {
			a.st.Refresh()
		}
	})
}

// askCard shows a question a thread's specialist put to the owner: its
// suggested answers as buttons, and a field for any other answer. The
// answer goes to the asking session.
func (a *App) askCard(c *ui.Context, ask proto.Ask) {
	t := c.Theme()
	key := "ask:" + itoa(int(ask.ID))
	pending := a.st.Answering(ask.ID)
	text := a.answers[ask.ID]
	if text == nil {
		text = new(string)
		a.answers[ask.ID] = text
	}
	ui.Column(c.Key(key)).Gap(frostui.SpaceS).Padding(frostui.SpaceM, frostui.SpaceL).AlignItems(ui.Stretch).
		Radius(10).Background(frostui.Panel(t)).BorderWidth(1, 1, 1, 1).BorderColor(frostui.ToneColor(t, frostui.Accent)).
		Role(ui.RoleGroup).Label("Question A" + itoa(int(ask.ID))).Children(func() {
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			frostui.Icon(c, "help-browser").TextColor(frostui.ToneColor(t, frostui.Accent))
			frostui.Strong(c, "A"+itoa(int(ask.ID))+" asks you").Grow(1).SingleLine()
			title := "Thread T" + itoa(int(ask.Thread))
			if s, ok := a.st.Data.Session(ask.SessionID); ok {
				title = s.Title
			}
			frostui.Caption(c, title).SingleLine()
			if ui.Button(c, "Open Session").Clicked() {
				a.openSession(ask.SessionID)
			}
		})
		ui.Text(c, ask.Question).Wrap().Selectable()
		if len(ask.Options) > 0 {
			ui.Row(c).Gap(frostui.SpaceS).Wrap().Children(func() {
				// Keyed by place: an older pocketd may repeat an option.
				for i, option := range ask.Options {
					if ui.Button(c.Key(key+":option:"+itoa(i)), option).Disabled(pending.Busy).Tooltip("Answer “" + option + "”").Clicked() {
						a.st.Answer(ask.ID, option)
					}
				}
			})
		}
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			placeholder := "Your answer"
			if len(ask.Options) > 0 {
				placeholder = "Or another answer"
			}
			field := ui.TextInput(c, text).Placeholder(placeholder).Label("Answer to A" + itoa(int(ask.ID))).
				Disabled(pending.Busy).Grow(1)
			if ui.PrimaryButton(c, "Answer").Disabled(pending.Busy || strings.TrimSpace(*text) == "").Clicked() || field.Submitted() {
				a.st.Answer(ask.ID, *text)
			}
		})
		if pending.Busy {
			frostui.Loading(c, "Sending your answer…")
		}
		if pending.Err != "" {
			frostui.Problem(c, pending.Err).Wrap()
		}
	})
}

// lessonCard shows a lesson that waits for the owner (ADR-0015): a new
// lesson, new text for one in force, or forgetting one. Approve approves
// exactly the text the card shows; Discard drops what waits, and a lesson
// in force stays.
func (a *App) lessonCard(c *ui.Context, lesson proto.Lesson) {
	t := c.Theme()
	// The card, and its buttons, are the proposal it shows: one the agent
	// changed meanwhile is another card, so a click on the old one never
	// approves the new text.
	seen := fnv.New64a()
	_, _ = seen.Write([]byte(state.LessonSeen(lesson)))
	key := "lesson:" + itoa(int(lesson.ID)) + ":" + strconv.FormatUint(seen.Sum64(), 36)
	pending := a.st.DecidingLesson(lesson.ID)
	name := "L" + itoa(int(lesson.ID))
	agent := lesson.Agent
	for _, p := range a.st.Data.Profiles() {
		if p.ID == lesson.Agent && p.Name != "" {
			agent = p.Name
		}
	}
	text, forget := state.SeenText(lesson)
	var title string
	switch {
	case lesson.State == proto.LessonPending:
		title = "A lesson for " + agent + " waits for your approval"
	case forget:
		title = "Forgetting " + name + " of " + agent + " waits for your approval"
	default:
		title = "New text for " + name + " of " + agent + " waits for your approval"
	}
	ui.Column(c.Key(key)).Gap(frostui.SpaceS).Padding(frostui.SpaceM, frostui.SpaceL).AlignItems(ui.Stretch).
		Radius(10).Background(frostui.Panel(t)).BorderWidth(1, 1, 1, 1).BorderColor(frostui.ToneColor(t, frostui.Accent)).
		Role(ui.RoleGroup).Label("Lesson " + name).Children(func() {
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			frostui.Icon(c, "accessories-text-editor").TextColor(frostui.ToneColor(t, frostui.Accent))
			frostui.Strong(c, title).Grow(1).SingleLine()
			frostui.Caption(c, name).SingleLine()
		})
		if lesson.State == proto.LessonActive {
			frostui.Caption(c, "In force now:")
			ui.Text(c, lesson.Text).Wrap().Selectable()
		}
		if !forget {
			if lesson.State == proto.LessonActive {
				frostui.Caption(c, "Proposed text:")
			}
			ui.Row(c).Padding(frostui.SpaceXS, frostui.SpaceS).Radius(6).Background(frostui.ColorsOf(t).Code).Children(func() {
				ui.Text(c, text).Wrap().Selectable()
			})
		}
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			ui.Spacer(c)
			if ui.Button(c.Key(key+":discard"), "Discard").Disabled(pending.Busy).Tooltip("Drop what waits; a lesson in force stays").Clicked() {
				a.st.DecideLesson(lesson, proto.LessonDiscard)
			}
			if ui.PrimaryButton(c.Key(key+":approve"), "Approve").Disabled(pending.Busy).Tooltip("Approve exactly what this card shows").Clicked() {
				a.st.DecideLesson(lesson, proto.LessonApprove)
			}
		})
		if pending.Busy {
			frostui.Loading(c, "Sending your decision…")
		}
		if pending.Err != "" {
			frostui.Problem(c, pending.Err).Wrap()
		}
	})
}

// approvalCard shows a tool call that waits for the owner, with Allow and
// Deny and an optional reason for a denial. inInbox adds the session it
// belongs to and a way there.
func (a *App) approvalCard(c *ui.Context, ap proto.Approval, inInbox bool) {
	t := c.Theme()
	pending := a.st.Deciding(ap.ID)
	reason := a.reasons[ap.ID]
	if reason == nil {
		reason = new(string)
		a.reasons[ap.ID] = reason
	}
	summary := state.ToolSummary(ap.Tool, ap.Arguments)
	ui.Column(c.Key(ap.ID)).Gap(frostui.SpaceS).Padding(frostui.SpaceM, frostui.SpaceL).AlignItems(ui.Stretch).
		Radius(10).Background(frostui.Panel(t)).BorderWidth(1, 1, 1, 1).BorderColor(frostui.ToneColor(t, frostui.Warning)).
		Role(ui.RoleGroup).Label("Approval: " + ap.Tool).Children(func() {
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			frostui.Icon(c, "dialog-warning").TextColor(frostui.ToneColor(t, frostui.Warning))
			frostui.Strong(c, ap.Tool+" waits for your approval").Grow(1).SingleLine()
			if inInbox {
				title := "Session"
				if s, ok := a.st.Data.Session(ap.SessionID); ok {
					title = s.Title
				}
				frostui.Caption(c, title).SingleLine()
				if ui.Button(c, "Open Session").Clicked() {
					a.openSession(ap.SessionID)
				}
			}
		})
		if summary != "" {
			ui.Row(c).Padding(frostui.SpaceXS, frostui.SpaceS).Radius(6).Background(frostui.ColorsOf(t).Code).Children(func() {
				frostui.Mono(c, summary).Wrap().Selectable()
			})
		}
		if full := state.IndentArguments(ap.Arguments); full != "" {
			a.approvalArguments(c, ap.ID, full)
		}
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			field := ui.TextInput(c, reason).Placeholder("Reason for denying (optional)").Label("Reason for denying").
				Disabled(pending.Busy).Grow(1)
			if ui.Button(c, "Deny").Disabled(pending.Busy).Clicked() || field.Submitted() {
				a.st.Decide(ap.ID, false, *reason)
			}
			if ui.PrimaryButton(c, "Allow").Disabled(pending.Busy).Clicked() {
				a.st.Decide(ap.ID, true, "")
			}
		})
		if pending.Busy {
			frostui.Loading(c, "Sending your decision…")
		}
		if pending.Err != "" {
			frostui.Problem(c, pending.Err).Wrap()
		}
	})
}

// approvalArguments folds a call's arguments: open, a preview
// (state.ArgumentsPreview), and when the preview is cut, Show All, which
// shows every byte in a read-only, scrolling field, and Copy All. The owner
// can read all of what a call would do before allowing it.
func (a *App) approvalArguments(c *ui.Context, id, full string) {
	open := a.argsOpen[id]
	ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
		if ui.ButtonBase(c).Row().Gap(frostui.SpaceXS).AlignItems(ui.Center).Cursor(ui.CursorPointer).
			Label("Arguments").Expanded(open).Children(func() {
			frostui.Icon(c, chevron(open))
			frostui.Muted(c, "Arguments")
		}).Clicked() {
			a.argsOpen[id] = !open
		}
		if open {
			ui.Spacer(c)
			if ui.Button(c, "Copy All").Tooltip("Copy every byte of the arguments").Clicked() {
				c.Services().WriteClipboard(full)
			}
		}
	})
	if !open {
		return
	}
	preview, cut := state.ArgumentsPreview(full)
	all := a.argsAll[id]
	switch {
	case all != nil:
		ui.TextAreaBase(c.Key(id+":all"), all).ReadOnly(true).Font("monospace").Lines(6, 24).
			Label("Complete arguments").Padding(frostui.SpaceS, frostui.SpaceM).Radius(6).Background(frostui.ColorsOf(c.Theme()).Code)
		ui.Row(c).Children(func() {
			if ui.Button(c, "Show Less").Clicked() {
				delete(a.argsAll, id)
			}
		})
	case cut:
		codeBox(c, preview, 240)
		ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			frostui.Caption(c, "The preview shows "+itoa(len(preview))+" of "+itoa(len(full))+" bytes.").Grow(1)
			if ui.Button(c, "Show All Arguments").Clicked() {
				a.argsAll[id] = &full
			}
		})
	default:
		codeBox(c, full, 240)
	}
}

func chevron(open bool) string {
	if open {
		return "pan-down"
	}
	return "pan-end"
}
