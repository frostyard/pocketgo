package profiles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/service/internal/schedule"
)

// SchedulesFile is the owner's schedules, in the profile folder, which
// agents' commands see read-only (ADR-0030).
const SchedulesFile = "schedules.json"

// ScheduleEntry is one of the owner's schedules, as read. An entry that
// breaks a rule has its Problem, and goes out never; the others go out
// whatever it says.
type ScheduleEntry struct {
	// N is the entry's place in the file, from 1.
	N int
	// ID names the schedule for good: pocketd keeps when it last went out
	// by it.
	ID string
	// When is when it goes out (schedule.Parse), and Spec what it says.
	When string
	Spec schedule.Spec
	// Zone is the IANA time zone of its clock times, "" for the service's.
	Zone string
	// Profile is the agent whose standing session it goes to.
	Profile string
	// Its action is exactly one of Message, the text the session gets,
	// after schedule.Prefix, and Gather, a gather pocketd runs for the
	// session's agent (ADR-0033).
	Message string
	Gather  *GatherAction
	// Problem says why the entry goes out never, "" for none.
	Problem string
}

// GatherAction is a schedule's gather (ADR-0033): the skill whose text
// each asked agent gets, the agents (every one the supervisor can delegate
// to when none), and a note to them.
type GatherAction struct {
	Skill  string
	Agents []string
	Note   string
}

// Schedules are the owner's schedules, in the file's order.
type Schedules struct {
	Entries []ScheduleEntry
}

// scheduleID is an entry's id.
var scheduleID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// LoadSchedules reads the owner's schedules in dir: none when there is no
// file. A file that is no regular file of its own, or is not the object
// ParseSchedules reads, is an error: none of it goes out.
//
// The folder is opened first, as Folder.Schedules opens it: a folder that
// is not there is an error, never a missing file.
func LoadSchedules(dir string) (Schedules, error) {
	return Folder{Dir: dir}.Schedules()
}

// ParseSchedules reads the owner's schedules (ADR-0030):
//
//	{"schedules": [{"id": "evening", "when": "every day 19:00", "zone": "Europe/Berlin",
//	  "profile": "supervisor", "message": "…"}, …]}
//
// Every object's keys are read as written, since a decoder matches a key
// in any case and keeps the last of one named twice. A file that is not
// that object is an error. An entry that breaks a rule is kept with its
// problem, and the others go out: its id, its when, its zone, its agent's
// id, its action (exactly one, a message or a gather), or a field it
// does not take. Two entries whose ids are the same in any case both have a
// problem, since each names the other's record.
func ParseSchedules(data []byte) (Schedules, error) {
	const shape = `not {"schedules": [{"id", "when", "profile", "message"}, …]}`
	if len(data) > maxProfileSize {
		return Schedules{}, fmt.Errorf("the file is larger than %d bytes", maxProfileSize)
	}
	if !utf8.Valid(data) {
		return Schedules{}, errors.New("the file is not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var raws []json.RawMessage
	seen := false
	err := readObject(dec, "the file", func(key string) error {
		if key != "schedules" {
			return fmt.Errorf("%s: it has the field %q", shape, key)
		}
		seen = true
		return readArray(dec, "schedules", func() error {
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return fmt.Errorf("schedule %d: %w", len(raws)+1, err)
			}
			raws = append(raws, raw)
			return nil
		})
	})
	if err != nil {
		return Schedules{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Schedules{}, errors.New("text follows the file's object")
	}
	if !seen {
		return Schedules{}, fmt.Errorf("%s: it has no schedules", shape)
	}
	out := Schedules{Entries: make([]ScheduleEntry, len(raws))}
	for i, raw := range raws {
		out.Entries[i] = readSchedule(raw, i+1)
	}
	// An id named twice names one record: neither entry goes out.
	first := map[string]int{}
	for i, e := range out.Entries {
		if e.ID == "" || !scheduleID.MatchString(e.ID) {
			continue
		}
		id := strings.ToLower(e.ID)
		if j, ok := first[id]; ok {
			share := fmt.Sprintf("schedules %d and %d share the id %s", j+1, i+1, e.ID)
			for _, k := range []int{j, i} {
				if out.Entries[k].Problem == "" {
					out.Entries[k].Problem = share
				}
			}
			continue
		}
		first[id] = i
	}
	return out, nil
}

// readSchedule reads entry n, and says what breaks a rule in its Problem.
func readSchedule(raw json.RawMessage, n int) ScheduleEntry {
	e := ScheduleEntry{N: n}
	where := fmt.Sprintf("schedule %d", n)
	dec := json.NewDecoder(bytes.NewReader(raw))
	actions := 0
	err := readObject(dec, where, func(key string) error {
		switch key {
		case "id":
			return decodeString(dec, where, key, &e.ID)
		case "when":
			return decodeString(dec, where, key, &e.When)
		case "zone":
			return decodeString(dec, where, key, &e.Zone)
		case "profile":
			return decodeString(dec, where, key, &e.Profile)
		case "message":
			actions++
			return decodeString(dec, where, key, &e.Message)
		case "gather":
			actions++
			g, err := readGather(dec, where+"'s gather")
			e.Gather = &g
			return err
		}
		return fmt.Errorf("%s has the field %q: an entry takes id, when, zone, profile, and a message or a gather", where, key)
	})
	if err != nil {
		// pocketd keeps the entry's record by its id whatever else breaks
		// it, so a field before the id that breaks a rule never drops the
		// record, nor hides a twin of its id (Codex review of #51, round 1).
		e.ID = scheduleIDOf(raw)
		e.Problem = err.Error()
		return e
	}
	switch {
	case e.ID == "":
		e.Problem = where + " has no id"
	case !scheduleID.MatchString(e.ID):
		e.Problem = where + "'s id is not 1 to 64 letters, digits, dots, dashes and underscores, starting with a letter or a digit"
	case e.When == "":
		e.Problem = where + " has no when: " + schedule.Help
	case e.Profile == "":
		e.Problem = where + " names no profile: the agent whose standing session it goes to"
	case !idPattern.MatchString(e.Profile):
		e.Problem = where + "'s profile is no profile's id"
	case actions == 0:
		e.Problem = where + " has no action: a message or a gather"
	case actions > 1:
		e.Problem = where + " has two actions: a message or a gather, not both"
	}
	if e.Problem != "" {
		return e
	}
	spec, err := schedule.Parse(e.When)
	if err != nil {
		e.Problem = fmt.Sprintf("%s's when: %v", where, err)
		return e
	}
	e.Spec = spec
	if _, err := schedule.Zone(e.Zone, nil); err != nil {
		e.Problem = fmt.Sprintf("%s: %v", where, err)
		return e
	}
	if e.Gather != nil {
		if err := checkGatherAction(*e.Gather); err != nil {
			e.Problem = fmt.Sprintf("%s's gather: %v", where, err)
		}
		return e
	}
	text, err := schedule.CheckText(e.Message)
	if err != nil {
		e.Problem = fmt.Sprintf("%s's message: %v", where, err)
		return e
	}
	e.Message = text
	return e
}

// readGather reads a schedule's gather: {"skill", "agents", "note"}, each
// key as written.
func readGather(dec *json.Decoder, where string) (GatherAction, error) {
	var g GatherAction
	err := readObject(dec, where, func(key string) error {
		switch key {
		case "skill":
			return decodeString(dec, where, key, &g.Skill)
		case "note":
			return decodeString(dec, where, key, &g.Note)
		case "agents":
			return readArray(dec, where+"'s agents", func() error {
				var agent string
				if err := decodeString(dec, where+"'s agents", "agent", &agent); err != nil {
					return err
				}
				g.Agents = append(g.Agents, agent)
				return nil
			})
		}
		return fmt.Errorf("%s has the field %q: a gather takes skill, agents and note", where, key)
	})
	return g, err
}

// checkGatherAction checks a schedule's gather: a skill's name, distinct
// agents by id, and a note of at most MaxGatherNote characters. Whether
// the skill is in the skills folder is read at the gather's time.
func checkGatherAction(g GatherAction) error {
	if err := CheckSkillName(g.Skill); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, agent := range g.Agents {
		if !idPattern.MatchString(agent) {
			return fmt.Errorf("%q is no agent's id", agent)
		}
		if seen[agent] {
			return fmt.Errorf("%s is named twice", agent)
		}
		seen[agent] = true
	}
	g.Note = strings.TrimSpace(g.Note)
	if n := utf8.RuneCountInString(g.Note); n > MaxGatherNote {
		return fmt.Errorf("the note has %d characters, more than %d", n, MaxGatherNote)
	}
	return nil
}

// scheduleIDOf is the id of an entry that breaks a rule: its one "id" key,
// as written, when its value is text; "" for none, for one named twice, and
// for an entry that is no object.
func scheduleIDOf(raw json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	id, seen := "", 0
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return ""
		}
		if key, _ := tok.(string); key == "id" {
			seen++
			if len(value) == 0 || value[0] != '"' || json.Unmarshal(value, &id) != nil {
				id = ""
			}
		}
	}
	if seen != 1 {
		return ""
	}
	return id
}

// Schedules reads the folder's schedules file, through the folder pocketd
// pinned at start (read).
func (f Folder) Schedules() (Schedules, error) {
	data, err := f.read(SchedulesFile)
	if errors.Is(err, fs.ErrNotExist) {
		return Schedules{}, nil
	}
	if err != nil {
		return Schedules{}, fmt.Errorf("%s: %w", SchedulesFile, err)
	}
	s, err := ParseSchedules(data)
	if err != nil {
		return Schedules{}, fmt.Errorf("%s: %w", SchedulesFile, err)
	}
	return s, nil
}
