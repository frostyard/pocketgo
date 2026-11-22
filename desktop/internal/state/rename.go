package state

import (
	"context"
	"fmt"
	"strings"

	"github.com/frostyard/pocketgo/proto"
)

// The owner renames a private agent's session (ADR-0058): from its header,
// where the title becomes a field, and from its sidebar row's menu. Only a
// session pocketd says may be renamed (Session.Renamable) offers either;
// pocketd checks its rule again at the rename (protocol rule 68).

// Renamable says whether the owner may rename session id: pocketd's copy
// of it says so. A session the client does not know is not.
func (d *Data) Renamable(id int64) bool {
	s, ok := d.Session(id)
	return ok && s.Renamable
}

// RenameOf is the session sidebar row k renames from its right-click
// menu (Rename…), and F2 on it: a session's row whose session may be
// renamed, nested or not; false for any other row, which offers no
// Rename… at all.
func (d *Data) RenameOf(k SidebarKey) (int64, bool) {
	id, ok := PinOf(k)
	if !ok || !d.Renamable(id) {
		return 0, false
	}
	return id, true
}

// ValidateTitle checks a session's new title as pocketd would (protocol
// rule 68): at most MaxTitleSize bytes as sent.
func ValidateTitle(title string) error {
	if len(title) > MaxTitleSize {
		return fmt.Errorf("the title has %d bytes, more than the %d a title may have", len(title), MaxTitleSize)
	}
	return nil
}

// TitleEdit is the owner's edit of a session's title in its header: Enter
// or the focus leaving the field finishes it, Escape cancels it. The zero
// value edits nothing.
type TitleEdit struct {
	// Session is the session whose title is edited, 0 for none.
	Session int64
	// Draft is the title as the owner types it.
	Draft string
	// from is the title the edit started from.
	from string
}

// Start edits session s's title, from the title it shows.
func (e *TitleEdit) Start(s proto.Session) {
	*e = TitleEdit{Session: s.ID, Draft: s.Title, from: s.Title}
}

// Editing says whether session id's title is being edited.
func (e *TitleEdit) Editing(id int64) bool { return id != 0 && e.Session == id }

// Cancel ends the edit and sends nothing, as Escape does.
func (e *TitleEdit) Cancel() { *e = TitleEdit{} }

// Finish ends the edit, as Enter or the focus leaving the field does: the
// session and the title to send, trimmed, "" for pocketd's default. send
// is false when the title would be the one the edit started from, the
// default included, and for no edit: nothing goes then.
func (e *TitleEdit) Finish() (id int64, title string, send bool) {
	if e.Session == 0 {
		return 0, "", false
	}
	id, title, from := e.Session, strings.TrimSpace(e.Draft), e.from
	*e = TitleEdit{}
	shown := title
	if shown == "" {
		shown = proto.DefaultTitle(id)
	}
	return id, title, shown != from
}

// Renamed applies the answer of POST /v1/sessions/{id}/title, as the
// session's event would: an event that changed the session since f began
// wins over the answer's copy.
func (d *Data) Renamed(f Fetch, s proto.Session) { d.Created(f, s) }

// titleKey is the request ids' key of session id's renames.
func titleKey(id int64) string { return fmt.Sprintf("title:%d", id) }

// Rename renames session id (POST /v1/sessions/{id}/title): title is
// trimmed, and "" asks for pocketd's default. Renaming is where it
// stands; a title over MaxTitleSize bytes fails there and goes nowhere. A
// rename tried again unchanged keeps its request id until the session is
// seen with another title than it had when the rename went
// (settleRenames): then the rename took effect, or another came after it,
// and the same title again is a new request, which pocketd would
// otherwise answer with the session as it is. Another title is a new
// request.
func (s *Store) Rename(id int64, title string) {
	p := pending(s.renames, id)
	if p.Busy {
		return
	}
	title = strings.TrimSpace(title)
	if err := ValidateTitle(title); err != nil {
		p.Err = err.Error()
		return
	}
	key := titleKey(id)
	s.settleRenames()
	s.renameFrom[id] = &keptTitle{from: s.shownTitle(id)}
	request := proto.RenameSession{RequestID: s.ids.For(key, title), Title: title}
	p.Busy, p.Err = true, ""
	s.log(LevelCommand, "Rename session %d", id)
	var session proto.Session
	f := s.Data.Begin()
	s.do(fmt.Sprintf("Renaming session %d", id), func(ctx context.Context, c *proto.Client) (err error) {
		session, err = c.RenameSession(ctx, id, request)
		return err
	}, func(err error) {
		p.Busy = false
		if err != nil {
			p.Err = err.Error()
			s.settleRenames()
			return
		}
		s.ids.Done(key)
		delete(s.renameFrom, id)
		s.Data.Renamed(f, session)
	})
}

// keptTitle is where a rename that keeps its request id stands: the title
// the session showed when it went, and whether the session was seen with
// another since.
type keptTitle struct {
	from  string
	moved bool
}

// shownTitle is the title session id shows now.
func (s *Store) shownTitle(id int64) string {
	shown, _ := s.Data.Session(id)
	return shown.Title
}

// settleRenames notes, for each rename that keeps its request id, whether
// the session was seen with another title than when it went, at any
// moment since, as settleModels does for a model change: once it was and
// the rename is not on its way, its id goes.
func (s *Store) settleRenames() {
	for id, k := range s.renameFrom {
		if !k.moved && s.shownTitle(id) != k.from {
			k.moved = true
		}
		if k.moved && !s.Renaming(id).Busy {
			s.ids.Done(titleKey(id))
			delete(s.renameFrom, id)
		}
	}
}

// Renaming is where the owner's rename of session id stands.
func (s *Store) Renaming(id int64) Pending { return valueOf(s.renames[id]) }
