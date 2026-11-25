package nsgoclient

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
	"github.com/nsgo-dev/nsgo/ui/chat"
)

func (c *client) conversation(cx *core.Scope, id int64) core.Node {
	c.store.LoadModels()
	view := chat.NewConversationState(cx)
	rows := core.NewMemoFunc(cx, func() []state.Row {
		c.revision.Get()
		o := c.store.Data.Open()
		if o == nil || o.ID != id {
			return nil
		}
		return state.BuildRows(o.Entries, o.Live, c.store.Data.ApprovalsFor(id), c.store.Data.ComposerOf(id).Question)
	}, nil)
	transcript := c.transcript(cx, id, rows.Get, view)
	questionID := core.NewMemo(cx, func() string {
		c.revision.Get()
		if q, ok := c.store.Data.Question(id); ok {
			return q.ID
		}
		return ""
	})
	readOnly := core.NewMemo(cx, func() string {
		c.revision.Get()
		if ro, why := c.store.Data.ReadOnly(id); ro {
			return why
		}
		return ""
	})
	asks := workspaceui.ConversationEntries[proto.Ask]{
		Items: func() []proto.Ask {
			c.revision.Get()
			var asks []proto.Ask
			for _, a := range c.store.Data.Asks() {
				if a.SessionID == id {
					asks = append(asks, a)
				}
			}
			return asks
		},
		Key:     func(a proto.Ask) string { return state.AskCard(a.ID) },
		Content: c.askCard,
	}
	return workspaceui.Conversation{State: view, Transcript: []core.Node{
		// A transcript that did not load says so, and one that loaded and
		// then failed to load again says it may be out of date: each with
		// a way to load it again.
		core.Dynamic(func(*core.Scope) core.Node {
			c.revision.Get()
			o := c.store.Data.Open()
			if o == nil || o.ID != id {
				return core.Group{}
			}
			again := func() { c.store.Open(id) }
			switch {
			case o.Err != "" && !o.Loaded:
				return c.problemBanner(workspaceui.Danger, "Loading the session failed: "+o.Err, again)
			case o.Err != "":
				return c.problemBanner(workspaceui.Warning, "This transcript may be out of date: loading it again failed: "+o.Err, again)
			case !o.Loaded:
				return core.Text{Content: "Loading conversation…", Style: ui.TypeCaption}
			}
			return core.Group{}
		}),
		transcript, asks, c.runFailures(id), c.answerCheck(id)},
		Footer: core.Func(func(cx *core.Scope) core.Node { return c.messageComposer(cx, id, view) }),
		FooterMode: func() string {
			if why := readOnly.Get(); why != "" {
				return "readonly/" + why
			}
			return questionID.Get()
		},
		Alternate: func(cx *core.Scope, key string) core.Node {
			if why, ok := strings.CutPrefix(key, "readonly/"); ok {
				return body(why)
			}
			q, _ := c.store.Data.Question(id)
			return c.questionComposer(cx, q, view)
		}}
}
func (c *client) message(cx *core.Scope, row func() state.Row, view *chat.ConversationState) core.Node {
	r := core.Untracked(cx, row)
	text := func() string { return row().Text }
	// Copy shows that it copied for state.CopyHold.
	copyAction := chat.Actions{Receipt: state.CopyHold, Text: func() string { s, _ := row().Copy(); return s }}
	switch r.Kind {
	case state.RowTool:
		activity := chat.ActivityRow{Conversation: view, Activity: func() chat.Activity {
			return toolActivity(func() *state.ToolRow { return row().Tool })
		}}
		return workspaceui.Stack(activity, core.Dynamic(func(*core.Scope) core.Node {
			if id := row().Tool.ApprovalID; id != "" {
				return c.approvalActions(id)
			}
			return core.View{}
		}))
	case state.RowAssistant:
		return workspaceui.Stack(chat.Reasoning{Conversation: view, Text: func() string { return row().Thinking }, Status: func() chat.Status {
			if row().Live {
				return chat.Running
			}
			return chat.Completed
		}}, workspaceui.Message(false, c.markdown(text, view.Search()), copyAction))
	case state.RowUser:
		if r.FromService() {
			return chat.ActivityRow{Conversation: view, Activity: func() chat.Activity {
				f := state.FoldOf(text())
				return chat.Activity{ID: r.Key, Action: f.Head, Detail: f.Line, Status: chat.Completed, Content: func(*core.Scope) core.Node { return workspaceui.Message(false, c.markdown(text, view.Search())) }}
			}}
		}
		extras := []core.Node{}
		for _, f := range r.Files {
			extras = append(extras, core.Text{Content: f.Name + " · " + f.Path, Selectable: core.SelectableOn})
		}
		if r.Images > 0 {
			extras = append(extras, core.Text{Content: fmt.Sprintf("%d image(s)", r.Images)})
		}
		if r.Queued {
			extras = append(extras, core.Text{Content: "Queued"})
		}
		extras = append(extras, copyAction)
		if id, ok := r.FromSupervisor(); ok {
			// A supervisor's message says whose it is over its words
			// (ADR-0032).
			return workspaceui.Message(false, c.supervisorMark(id), append([]core.Node{c.markdown(text, view.Search())}, extras...)...)
		}
		return workspaceui.Message(r.Sender == nil, c.markdown(text, view.Search()), extras...)
	case state.RowQuestion:
		return c.questionRow(cx, row)
	case state.RowError:
		// A reply that failed is a problem, apart from the run's words.
		return core.Dynamic(func(*core.Scope) core.Node { return c.problemBanner(workspaceui.Danger, text(), nil) })
	case state.RowNote:
		return noteRow(cx, text)
	case state.RowCompaction:
		return chat.ActivityRow{Conversation: view, Activity: func() chat.Activity {
			return chat.Activity{ID: r.Key, Action: "Context compacted", Status: chat.Completed, Content: func(*core.Scope) core.Node { return workspaceui.Message(false, c.markdown(text, view.Search())) }}
		}}
	default:
		return core.Text{ContentFunc: func() string {
			if s := text(); s != "" {
				return s
			}
			return "Working…"
		}, Style: ui.TypeCaption, Selectable: core.SelectableOn}
	}
}
func (c *client) button(label string, fn func()) ui.Button {
	return ui.Button{Text: label, Variant: ui.ButtonSecondary, OnPress: func() { fn(); c.changed() }}
}
func (c *client) result(err error) {
	if err != nil {
		c.showError(err.Error())
	}
	c.changed()
}

// runFailures say, at the transcript's end, that the session's run
// failed, that compacting its context failed, or that starting over or
// renaming the session did.
func (c *client) runFailures(id int64) core.Node {
	return core.Dynamic(func(*core.Scope) core.Node {
		c.revision.Get()
		var out []core.Node
		if o := c.store.Data.Open(); o != nil && o.ID == id {
			if o.Live.Failure != "" {
				out = append(out, c.problemBanner(workspaceui.Danger, "The run failed: "+o.Live.Failure, nil))
			}
			if o.Live.CompactionFailure != "" {
				out = append(out, c.problemBanner(workspaceui.Warning, "Compacting the context failed: "+o.Live.CompactionFailure, nil))
			}
		}
		if why := c.store.StartingOver(id).Err; why != "" {
			out = append(out, c.problemBanner(workspaceui.Danger, "Starting over failed: "+why, nil))
		}
		if why := c.store.Renaming(id).Err; why != "" {
			// The title stays as pocketd last sent it (ADR-0058).
			out = append(out, c.problemBanner(workspaceui.Danger, "Renaming the session failed: "+why, nil))
		}
		return core.Group(out)
	})
}

func (c *client) messageComposer(cx *core.Scope, id int64, view *chat.ConversationState) core.Node {
	draft := c.drafts[id]
	if draft == nil {
		draft = core.NewSignal(c.cx, "")
		c.drafts[id] = draft
	}
	picker := &state.PromptPicker{}
	gen := c.generation
	pick := func(key state.PickerKey) bool {
		res := picker.Key(key, draft.Get(), c.store.Data.Prompts())
		if res.Insert {
			draft.Set(res.Text)
		}
		if res.Took {
			c.changed()
		}
		return res.Took
	}
	menu := core.NewMemoFunc(cx, func() state.PromptMenu {
		c.revision.Get()
		status, why := c.store.Data.PromptsStatus()
		return picker.View(draft.Get(), c.store.Data.Prompts(), status, why)
	}, nil)
	cx.Effect(func() {
		if menu.Get().Opened {
			c.store.LoadPrompts()
		}
	})
	valid := func() bool {
		ro, _ := c.store.Data.ReadOnly(id)
		return !ro && gen == c.generation && c.session.Get() == id && c.store.SignedIn() && c.store.Data.ComposerOf(id).Attaches()
	}
	send := func() {
		if !valid() || !c.store.MaySend(id) {
			return
		}
		if menu.Get().Open {
			picker.EnterPressed(false)
			if pick(state.PickerEnter) {
				return
			}
		}
		msg := draft.Get()
		if strings.TrimSpace(msg) == "" && len(c.store.Attachments(id)) == 0 {
			return
		}
		c.store.Send(id, msg, func(err error) {
			if err == nil && draft.Get() == msg {
				draft.Set("")
			}
			c.result(err)
		})
		c.changed()
	}
	attach := func(f core.File) {
		if valid() {
			if f.Path != "" {
				c.store.Attach(id, f.Name, state.FileSource(f.Path))
				c.changed()
				return
			}
			c.store.Attach(id, f.Name, state.Source{Open: func() (io.ReadCloser, int64, error) { r, err := f.Open(); return r, f.Size, err }})
			c.changed()
		}
	}
	drop := func(uris []string) {
		for _, u := range uris {
			f, err := core.FileFromURL(u)
			if err != nil {
				c.result(err)
				continue
			}
			attach(f)
		}
	}
	problem := core.NewMemo(cx, func() string {
		c.revision.Get()
		return strings.Join(nonempty(c.store.Sending(id).Err, c.store.Aborting(id).Err, c.store.ChangingModel(id).Err), "\n")
	})
	// Keep mounted controls through store updates that do not change their
	// contents. Pointer events on both hosts target the mounted control.
	type promptList struct {
		open          bool
		items, broken []proto.Prompt
		note          string
	}
	displayPrompts := core.NewMemoFunc(cx, func() promptList {
		m := menu.Get()
		return promptList{m.Open, m.Items, m.Broken, state.PromptNote(m)}
	}, func(a, b promptList) bool {
		return a.open == b.open && a.note == b.note && slices.Equal(a.items, b.items) && slices.Equal(a.broken, b.broken)
	})
	refusals := core.NewMemoFunc(cx, func() []state.QuestionRefusal {
		c.revision.Get()
		return c.store.QuestionRefusals(id)
	}, slices.Equal[[]state.QuestionRefusal])
	attachments := core.NewMemoFunc(cx, func() []state.Attachment {
		c.revision.Get()
		return c.store.Attachments(id)
	}, slices.Equal[[]state.Attachment])
	return chat.Composer{State: view, Glass: true, Draft: draft.Get, OnDraftChange: draft.Set, Label: "Message", Placeholder: "Message this session…", SendLabel: "Send message", StopLabel: "Stop run", OnSend: send, OnStop: func() { c.store.Abort(id); c.changed() }, Running: func() bool { c.revision.Get(); o := c.store.Data.Open(); return o != nil && o.ID == id && o.Live.Busy }, SendDisabled: func() bool { c.revision.Get(); return !c.store.MaySend(id) }, StopDisabled: func() bool { c.revision.Get(); return c.store.Aborting(id).Busy },
		OnDropFiles: drop, OnPaste: func(p core.PasteEvent) {
			if !valid() {
				return
			}
			drop(p.URIs)
			for _, f := range p.Files {
				name := f.Name
				if name == "" {
					name = state.PastedImageName
				}
				c.store.Attach(id, name, state.BytesSource(f.Data))
			}
			c.changed()
		},
		Accessory: func(cx *core.Scope) core.Node {
			return core.View{Style: core.Style{Direction: core.Row, Wrap: core.Wrap, AlignItems: core.AlignCenter, Gap: 4, FlexGrow: 1, FlexShrink: 1, MinWidth: core.Pt(0)}, Children: []core.Node{chat.IconAction{Icon: lucide.Paperclip, Label: "Attach files", OnPress: func() {
				cx.PickFiles(core.PickFilesOptions{Title: "Attach files", Multiple: true}, func(files []core.File, err error) {
					if err != nil {
						c.result(err)
						return
					}
					for _, f := range files {
						attach(f)
					}
				})
			}}, chat.IconAction{Label: "Saved prompts", Icon: lucide.Slash, OnPress: func() { txt, _ := picker.Summon(draft.Get()); draft.Set(txt); c.changed() }}, c.modelControls(cx, id)}}
		},
		HasAttachments: func() bool {
			return problem.Get() != "" || displayPrompts.Get().open || len(refusals.Get()) > 0 || len(attachments.Get()) > 0
		}, Attachments: func(*core.Scope) core.Node {
			return core.Dynamic(func(cx *core.Scope) core.Node {
				var nodes []core.Node
				if msg := problem.Get(); msg != "" {
					nodes = append(nodes, core.Text{Content: msg, Selectable: core.SelectableOn, StyleFunc: ui.Colored(ui.ColorsOf(cx).Danger)})
				}
				if m := displayPrompts.Get(); m.open {
					for _, q := range m.items {
						nodes = append(nodes, c.button("/"+q.Name, func() {
							if txt, _, ok := picker.Choose(q.Name, draft.Get(), c.store.Data.Prompts()); ok {
								draft.Set(txt)
							}
							c.changed()
						}))
					}
					for _, q := range m.broken {
						nodes = append(nodes, body("/"+q.Name+" · "+q.Problem))
					}
					nodes = append(nodes, body(m.note), c.button("Close prompts", func() { pick(state.PickerEscape) }))
					for _, k := range []struct {
						name string
						key  state.PickerKey
					}{{"up", state.PickerUp}, {"down", state.PickerDown}, {"tab", state.PickerTab}, {"escape", state.PickerEscape}} {
						cx.OnKeyWith(k.name, core.KeyOptions{When: core.KeyAlways}, func() { pick(k.key) })
					}
				}
				for _, r := range refusals.Get() {
					nodes = append(nodes, body(r.Why), body(r.Answer), c.button("Dismiss answer", func() { c.store.DismissQuestionRefusal(r.Question) }))
				}
				for _, f := range attachments.Get() {
					label := f.Name
					if f.Uploading {
						label += " · uploading"
					}
					nodes = append(nodes, core.Text{Content: label + " " + f.Err})
					if f.Retry && f.Err != "" {
						nodes = append(nodes, c.button("Retry", func() { c.store.RetryAttachment(id, f.Key) }))
					}
					nodes = append(nodes, c.button("Remove "+f.Name, func() { c.store.RemoveAttachment(id, f.Key) }))
				}
				if len(attachments.Get()) > 0 {
					nodes = append(nodes, ui.Button{Text: "Send attachments", Disabled: func() bool { c.revision.Get(); return !c.store.MaySend(id) }, OnPress: send})
				}
				return workspaceui.Row(nodes...)
			})
		},
		Trailing: func(cx *core.Scope) core.Node {
			fill := core.NewMemo(cx, func() state.ContextFill { c.revision.Get(); return c.store.Data.FillOf(id) }).Get
			return chat.ContextMeter{Label: "Context usage", Title: "Context usage", Tooltip: func() string { return fill().Tooltip() }, Value: func() string {
				if f := fill(); f.Known && f.Window > 0 {
					return fmt.Sprintf("%s / %s tokens (%s)", state.FormatTokens(f.Tokens), state.FormatTokens(int64(f.Window)), f.Label())
				}
				if v := fill().Label(); v != "" {
					return v
				}
				return "Awaiting measurement"
			}, Fraction: func() float64 {
				v, top, ok := fill().Bar()
				if !ok {
					return -1
				}
				return min(1, v/top)
			}}
		},
	}
}
func nonempty(ss ...string) []string {
	return slices.DeleteFunc(ss, func(s string) bool { return s == "" })
}
func (c *client) questionComposer(cx *core.Scope, q proto.Question, v *chat.ConversationState) core.Node {
	draft := c.store.QuestionDraft(q)
	value := core.NewSignal(cx, *draft)
	picks := core.NewSignalFunc(cx, []int(nil), slices.Equal[[]int])
	submit := func() { c.store.AnswerQuestion(q, value.Get(), picks.Get(), c.result); c.changed() }
	var options []core.Node
	for i, s := range q.Options {
		options = append(options, ui.Button{TextFunc: func() string {
			if slices.Contains(picks.Get(), i) {
				return "✓ " + s
			}
			return s
		}, Variant: ui.ButtonSecondary, OnPress: func() {
			p := slices.Clone(picks.Get())
			if j := slices.Index(p, i); j >= 0 {
				p = slices.Delete(p, j, j+1)
			} else if q.Multiple {
				p = append(p, i)
			} else {
				p = []int{i}
			}
			picks.Set(p)
		}})
	}
	options = append(options, c.button("Skip question", func() { c.store.SkipQuestion(q) }))
	return workspaceui.Stack(workspaceui.Row(options...), chat.Composer{Glass: true, State: v, Draft: value.Get, OnDraftChange: func(s string) { *draft = s; value.Set(s) }, Label: "Answer question", Placeholder: "Answer the question…", OnSend: submit, Trailing: func(*core.Scope) core.Node {
		return core.Show{When: func() bool { return len(picks.Get()) > 0 }, Then: func(*core.Scope) core.Node { return c.button("Submit choices", submit) }}
	}})
}
