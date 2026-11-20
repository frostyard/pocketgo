package team

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// A thread's worker (ADR-0021): its specialist sends it work with
// team_worker, and its replies come back as messages.

// WorkerMessage is a team_worker call: the brief of a new worker, or a
// follow-up to it.
type WorkerMessage struct {
	Message string `json:"message"`
}

// MaxWorkerMessage is the most characters a message to a worker has: a
// full brief, with the plan and the files to change.
const MaxWorkerMessage = 20000

// MaxWorkerReply is the most characters of a worker's reply that reach its
// specialist; a longer one is clipped, and says so.
const MaxWorkerReply = 8000

// CheckWorkerMessage refuses an empty message, and one too long.
func CheckWorkerMessage(w WorkerMessage) error {
	text := strings.TrimSpace(w.Message)
	switch {
	case text == "":
		return errors.New("the message is empty: give the worker its task in full")
	case utf8.RuneCountInString(text) > MaxWorkerMessage:
		return fmt.Errorf("the message has %d characters, more than %d", utf8.RuneCountInString(text), MaxWorkerMessage)
	}
	return nil
}

// WorkerReplyText is the message a worker's reply brings its thread's
// specialist: the reply, clipped to MaxWorkerReply.
func WorkerReplyText(thread int64, reply string) string {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		reply = "(The worker ended its run with no reply.)"
	}
	if utf8.RuneCountInString(reply) > MaxWorkerReply {
		reply = string([]rune(reply)[:MaxWorkerReply]) + "\n\n[pocketd clipped the reply at " + fmt.Sprint(MaxWorkerReply) + " characters; ask the worker for the rest]"
	}
	return fmt.Sprintf("[T%d · worker] %s", thread, reply)
}

// WorkerSent is team_worker's answer: the message went to the worker.
func WorkerSent(thread, session int64, model string, started bool) string {
	what := "Sent to your worker"
	if started {
		what = "Started your worker and sent it the brief"
	}
	return fmt.Sprintf("%s (session %d, on %s). Its reply comes to you as a message that starts with [T%d · worker]: wait for it, then check its work "+
		"(git diff in your folder) before you send fixes or report.", what, session, model, thread)
}

// WorkerInstructions are a worker's instructions, after the folder's: it
// works for one thread's specialist, and answers it alone.
const WorkerInstructions = "You are a worker on a team: a thread's specialist sends you tasks, and checks your work. " +
	"Do exactly the task in its message, in your folder, with your tools. Do not ask questions; when something blocks you, say what and stop. " +
	"Do not commit, push, open pull requests or change anything outside your folder unless the task says to. " +
	"End your run with a short reply for the specialist: what you changed, which files, what you ran and its result, and what is left. " +
	"That reply is the only thing the specialist reads."
