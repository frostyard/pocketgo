package state

import "github.com/frostyard/pocketgo/proto"

// requestIDs gives each state-changing action its request id. An action
// that failed keeps its id, so trying the same action again sends the same
// id and the service applies it at most once (protocol rule 5), even when
// the first try reached the service and only its answer was lost. A changed
// action, or one tried after a success, gets a new id. The zero value is
// ready.
type requestIDs struct {
	kept map[string]keptID
	// newID makes an id; nil is proto.NewRequestID. Tests set it.
	newID func() string
}

type keptID struct {
	action string // what the id was made for
	id     string
}

// For returns the request id for action under key, such as the text of a
// message under "send:<session>": the kept one when the last try under key
// was the same action and did not succeed, else a new one.
func (r *requestIDs) For(key, action string) string {
	if k, ok := r.kept[key]; ok && k.action == action {
		return k.id
	}
	newID := r.newID
	if newID == nil {
		newID = proto.NewRequestID
	}
	if r.kept == nil {
		r.kept = map[string]keptID{}
	}
	id := newID()
	r.kept[key] = keptID{action: action, id: id}
	return id
}

// Done forgets the id under key, after its action succeeded or can no
// longer apply.
func (r *requestIDs) Done(key string) { delete(r.kept, key) }

// reset forgets every id, as after signing out.
func (r *requestIDs) reset() { r.kept = nil }
