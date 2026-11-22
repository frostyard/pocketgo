package team

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// A question is 1 to 500 characters; it suggests no option or 2 to 6, of
// 60 characters each, none twice; multiple needs options; a placeholder
// has 120 characters at most. What is kept is trimmed (ADR-0048).
func TestCheckQuestion(t *testing.T) {
	got, err := CheckQuestion(Question{Question: "  Which database?\n", Options: []string{" Postgres ", "SQLite"}, Multiple: true, Placeholder: " or another "})
	if err != nil || got.Question != "Which database?" || !slices.Equal(got.Options, []string{"Postgres", "SQLite"}) || !got.Multiple || got.Placeholder != "or another" {
		t.Fatalf("CheckQuestion = %+v, %v", got, err)
	}
	if _, err := CheckQuestion(Question{Question: strings.Repeat("é", proto.MaxQuestion)}); err != nil {
		t.Errorf("a question of %d characters: %v", proto.MaxQuestion, err)
	}
	six := []string{"a", "b", "c", "d", "e", strings.Repeat("f", proto.MaxQuestionOption)}
	if _, err := CheckQuestion(Question{Question: "Pick", Options: six}); err != nil {
		t.Errorf("six options: %v", err)
	}
	for name, q := range map[string]Question{
		"empty":                  {Question: "  "},
		"too long":               {Question: strings.Repeat("x", proto.MaxQuestion+1)},
		"one option":             {Question: "Pick", Options: []string{"a"}},
		"seven options":          {Question: "Pick", Options: append(slices.Clone(six[:5]), "f", "g")},
		"an empty option":        {Question: "Pick", Options: []string{"a", " "}},
		"a long option":          {Question: "Pick", Options: []string{"a", strings.Repeat("b", proto.MaxQuestionOption+1)}},
		"an option twice":        {Question: "Pick", Options: []string{"a", " a"}},
		"multiple, no option":    {Question: "Pick", Multiple: true},
		"a placeholder too long": {Question: "Pick", Placeholder: strings.Repeat("p", proto.MaxQuestionPlaceholder+1)},
	} {
		if got, err := CheckQuestion(q); err == nil {
			t.Errorf("%s: passed as %+v", name, got)
		}
	}
}

// Only a supervisor's and a private agent's session, working as none of
// the roles, gets the question tool; any other is refused, a thread's
// specialist pointed to its asks.
func TestOnlyTheSupervisorAndAPrivateAgentMayQuestion(t *testing.T) {
	supervisor := &profiles.Profile{ID: "odrade", Role: profiles.RoleSupervisor}
	private := &profiles.Profile{ID: "chani", Role: profiles.RoleAgent, Private: true}
	agent := &profiles.Profile{ID: "vera", Role: profiles.RoleAgent}
	clerk := &profiles.Profile{ID: "clerk", Role: profiles.RoleClerk}
	for name, tc := range map[string]struct {
		p    *profiles.Profile
		role SessionRole
		want bool
	}{
		"the supervisor":           {supervisor, SessionRole{}, true},
		"a private agent":          {private, SessionRole{}, true},
		"an agent":                 {agent, SessionRole{}, false},
		"a clerk":                  {clerk, SessionRole{}, false},
		"no profile":               {nil, SessionRole{}, false},
		"a thread's specialist":    {agent, SessionRole{Thread: true}, false},
		"a supervisor in a thread": {supervisor, SessionRole{Thread: true}, false},
		"a review":                 {supervisor, SessionRole{Thread: true, Review: true}, false},
		"a worker":                 {nil, SessionRole{Worker: true}, false},
		"a private worker":         {private, SessionRole{Worker: true}, false},
		"a consult":                {supervisor, SessionRole{Consult: true}, false},
		"a gather's session":       {private, SessionRole{Gather: true}, false},
	} {
		if got := MayQuestion(tc.p, tc.role); got != tc.want {
			t.Errorf("%s: MayQuestion %v, want %v", name, got, tc.want)
		}
		if err := CheckQuestionCaller(tc.p, tc.role); (err == nil) != tc.want {
			t.Errorf("%s: CheckQuestionCaller %v", name, err)
		}
	}
	if err := CheckQuestionCaller(agent, SessionRole{Thread: true}); err == nil || !strings.Contains(err.Error(), "as asks, in its report with team_message") {
		t.Errorf("a thread's specialist: %v", err)
	}
}

// A run the owner's own message did not start is refused with the
// issue's words.
func TestCheckWatched(t *testing.T) {
	if err := CheckWatched(true); err != nil {
		t.Errorf("the owner's run: %v", err)
	}
	err := CheckWatched(false)
	if !errors.Is(err, ErrNobodyWatching) || err.Error() != "nobody is watching this run; ask in prose, in your summary" {
		t.Errorf("another run: %v", err)
	}
}

// A question waits 30 minutes, or the profile's questionMinutes.
func TestQuestionTimeout(t *testing.T) {
	if got := QuestionTimeout(profiles.Profile{}); got != 30*time.Minute {
		t.Errorf("the default %v", got)
	}
	if got := QuestionTimeout(profiles.Profile{QuestionMinutes: 45}); got != 45*time.Minute {
		t.Errorf("the profile's %v", got)
	}
}

// An answer is words, options picked, or both; a skip is neither. The
// options are indexes, none twice, more than one only with multiple, kept
// in order; with no words, the answer is the options' text.
func TestCheckAnswer(t *testing.T) {
	options := []string{"Postgres", "SQLite", "None"}
	for name, tc := range map[string]struct {
		multiple bool
		in       proto.AnswerQuestion
		answer   string
		option   []int
	}{
		"words":               {false, proto.AnswerQuestion{Answer: "  MySQL  "}, "MySQL", nil},
		"an option":           {false, proto.AnswerQuestion{Option: []int{1}}, "SQLite", []int{1}},
		"options, sorted":     {true, proto.AnswerQuestion{Option: []int{2, 0}}, "Postgres; None", []int{0, 2}},
		"words and an option": {false, proto.AnswerQuestion{Answer: "SQLite, for now", Option: []int{1}}, "SQLite, for now", []int{1}},
		"a skip":              {false, proto.AnswerQuestion{Skipped: true}, "", nil},
		"the longest words":   {false, proto.AnswerQuestion{Answer: strings.Repeat("é", proto.MaxAnswer)}, strings.Repeat("é", proto.MaxAnswer), nil},
	} {
		answer, option, err := CheckAnswer(options, tc.multiple, tc.in)
		if err != nil || answer != tc.answer || !slices.Equal(option, tc.option) {
			t.Errorf("%s: %q %v, %v", name, answer, option, err)
		}
	}
	for name, tc := range map[string]struct {
		options  []string
		multiple bool
		in       proto.AnswerQuestion
	}{
		"nothing":                 {options, false, proto.AnswerQuestion{}},
		"spaces":                  {options, false, proto.AnswerQuestion{Answer: "  "}},
		"too long":                {options, false, proto.AnswerQuestion{Answer: strings.Repeat("x", proto.MaxAnswer+1)}},
		"a skip with words":       {options, false, proto.AnswerQuestion{Skipped: true, Answer: "no"}},
		"a skip with an option":   {options, false, proto.AnswerQuestion{Skipped: true, Option: []int{0}}},
		"two of a single choice":  {options, false, proto.AnswerQuestion{Option: []int{0, 1}}},
		"an option out of range":  {options, true, proto.AnswerQuestion{Option: []int{3}}},
		"a negative option":       {options, true, proto.AnswerQuestion{Option: []int{-1}}},
		"an option twice":         {options, true, proto.AnswerQuestion{Option: []int{1, 1}}},
		"an option of none given": {nil, false, proto.AnswerQuestion{Option: []int{0}}},
	} {
		if answer, option, err := CheckAnswer(tc.options, tc.multiple, tc.in); err == nil {
			t.Errorf("%s: passed as %q %v", name, answer, option)
		}
	}
}

// The tool's result is JSON: the answer with the option picked, an index
// or a list for a multiple choice, or the skip with its reason.
func TestQuestionResult(t *testing.T) {
	for want, q := range map[string]proto.Question{
		`{"answer":"MySQL"}`:                         {State: proto.QuestionAnswered, Answer: "MySQL"},
		`{"answer":"SQLite","option":1}`:             {State: proto.QuestionAnswered, Answer: "SQLite", Option: []int{1}},
		`{"answer":"Postgres; None","option":[0,2]}`: {State: proto.QuestionAnswered, Answer: "Postgres; None", Option: []int{0, 2}, Multiple: true},
		`{"answer":"just this one","option":[1]}`:    {State: proto.QuestionAnswered, Answer: "just this one", Option: []int{1}, Multiple: true},
		`{"skipped":true,"reason":"owner"}`:          {State: proto.QuestionSkipped, Reason: proto.SkippedByOwner},
		`{"skipped":true,"reason":"timeout"}`:        {State: proto.QuestionSkipped, Reason: proto.SkippedTimeout},
		`{"answer":"say \"hi\"\nthen go"}`:           {State: proto.QuestionAnswered, Answer: "say \"hi\"\nthen go"},
		`{"skipped":true,"reason":"aborted"}`:        {State: proto.QuestionSkipped, Reason: proto.SkippedAborted, Answer: "ignored"},
	} {
		if got := QuestionResult(q); got != want {
			t.Errorf("QuestionResult(%+v) = %s, want %s", q, got, want)
		}
	}
}
