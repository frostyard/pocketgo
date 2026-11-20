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
	// next is the id the next new action under a key takes, made by Peek
	// before the action is sent, so what was shown from it is what goes.
	next map[string]string
	// newID makes an id; nil is proto.NewRequestID. Tests set it.
	newID func() string
}

type keptID struct {
	action string // what the id was made for
	id     string
}

// For returns the request id for action under key, such as the text of a
// message under "send:<session>": the kept one when the last try under key
// was the same action and did not succeed, else a new one, the one Peek
// showed when it showed one.
func (r *requestIDs) For(key, action string) string {
	if k, ok := r.kept[key]; ok && k.action == action {
		return k.id
	}
	id, ok := r.next[key]
	if ok {
		delete(r.next, key)
	} else {
		id = r.make()
	}
	if r.kept == nil {
		r.kept = map[string]keptID{}
	}
	r.kept[key] = keptID{action: action, id: id}
	return id
}

// Peek is the request id For would return for action under key now,
// without taking it: New Session shows the worktree a session's request
// id names before it sends (protocol rule 6a). For then returns the same
// id for the same action, and so does Peek until then.
func (r *requestIDs) Peek(key, action string) string {
	if k, ok := r.kept[key]; ok && k.action == action {
		return k.id
	}
	if id, ok := r.next[key]; ok {
		return id
	}
	if r.next == nil {
		r.next = map[string]string{}
	}
	r.next[key] = r.make()
	return r.next[key]
}

func (r *requestIDs) make() string {
	if r.newID == nil {
		return proto.NewRequestID()
	}
	return r.newID()
}

// Done forgets the id under key, after its action succeeded or can no
// longer apply.
func (r *requestIDs) Done(key string) { delete(r.kept, key) }

// reset forgets every id, as after signing out.
func (r *requestIDs) reset() { r.kept, r.next = nil, nil }
