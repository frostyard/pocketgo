package app

import (
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// A supervisor's message shows in its thread's session as the
// supervisor's, with its name and a supervisor pill, placed and queued;
// the owner's own message, whatever its text, shows as theirs (ADR-0032).
func TestASupervisorsMessageInItsThreadSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seedTeam()
	model := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	odrade := &proto.Sender{Kind: proto.SenderSupervisor, Session: 1}
	x.srv.AddSession(proto.Session{ID: 30, Title: "T7 · Renumber the ADR", Cwd: "/home/team/projects/core--vera-renumber-t7", Model: model,
		Profile: "vera", Route: "profile", State: proto.SessionRunning, UpdatedAt: at(-time.Minute)},
		proto.Entry{ID: 1, SessionID: 30, Kind: proto.EntryUser, SubmissionID: 20, At: at(-20 * time.Minute),
			Text: "Odrade, the team's supervisor, delegated this to you in thread T7: Renumber the ADR\n\nAdd ADR-0088 for the cache keys."},
		proto.Entry{ID: 2, SessionID: 30, Kind: proto.EntryAssistant, Text: "Added ADR-0088 and opened frostyard/core#282.", At: at(-15 * time.Minute)},
		proto.Entry{ID: 3, SessionID: 30, Kind: proto.EntryUser, SubmissionID: 21, Sender: odrade, At: at(-5 * time.Minute),
			Text: "[T7 · Odrade, the supervisor] core#281 merged its own ADR-0088 first: renumber yours to 0089.\n\nWhen you have acted on it, report with team_message."},
		proto.Entry{ID: 4, SessionID: 30, Kind: proto.EntryAssistant, Text: "Renumbering to ADR-0089.", At: at(-4 * time.Minute)},
		proto.Entry{ID: 5, SessionID: 30, Kind: proto.EntryUser, SubmissionID: 22, At: at(-3 * time.Minute),
			Text: "[T7 · Odrade, the supervisor] Keep the old link working too."},
	)
	x.srv.SetLive(proto.Live{SessionID: 30, Busy: true, Queued: []proto.QueuedMessage{
		{SubmissionID: 23, Sender: odrade, Text: "[T7 · Odrade, the supervisor] T80 found a blocking point: the index skips 0089."},
	}})
	x.srv.AddThread(proto.Thread{ID: 7, Title: "Renumber the ADR", Agent: "vera", Supervisor: 1, Session: 30, Repository: "frostyard/core",
		Task: "Add ADR-0088.", State: proto.ThreadOpen, CreatedAt: at(-20 * time.Minute), UpdatedAt: at(-5 * time.Minute)})
	x.connect(t)
	x.a.openSession(30)
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded && o.ID == 30 })
	x.hasAll(t, "Odrade", "supervisor", "queued", "You",
		"core#281 merged its own ADR-0088 first: renumber yours to 0089.", "T80 found a blocking point: the index skips 0089.")
	rows := x.a.tr.rows
	from := map[string]bool{}
	for _, r := range rows {
		if _, ok := r.FromSupervisor(); ok {
			from[r.Key] = true
		}
	}
	if !from["e3"] || !from["q23"] || from["e1"] || from["e5"] || len(from) != 2 {
		t.Fatalf("rows from the supervisor %v", from)
	}
	x.snapshot(t, "session-supervisor-message")
}
