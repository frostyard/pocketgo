package team

import "strings"

// The outbox keys of the messages that go only while their thread is open
// (ADR-0021, ADR-0024, ADR-0032): a worker's reply and a message to the
// worker, a shift's round and summary, and a supervisor's message. A close
// withdraws each still queued before it commits, and drops each not placed
// in the transaction that closes the thread, so none goes after it, also
// once a message reopens the thread (ADR-0061).
const (
	WorkerReplyPrefix   = "worker-reply:"
	WorkerMessagePrefix = "worker-msg:"
	ShiftRoundPrefix    = "shift-round:"
	ShiftSummaryPrefix  = "shift-summary:"
)

// DroppedAtClose says whether the outbox message of key goes only while
// its thread is open: a close drops it.
func DroppedAtClose(key string) bool {
	for _, prefix := range []string{WorkerReplyPrefix, WorkerMessagePrefix, ShiftRoundPrefix, ShiftSummaryPrefix, sendPrefix} {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}
