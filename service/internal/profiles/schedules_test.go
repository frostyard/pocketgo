package profiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The owner's schedules are checked in code: an entry that breaks a rule
// has its problem and goes out never, and the others go out (ADR-0030).
func TestParseSchedules(t *testing.T) {
	got, err := ParseSchedules([]byte(`{"schedules": [
		{"id": "evening-mon", "when": "every mon 19:00", "zone": "Europe/Berlin", "profile": "supervisor", "message": "  Run the features gather.\n"},
		{"id": "nudge", "when": "every 2h", "profile": "supervisor", "message": "Look at the desk."}
	]}`))
	if err != nil || len(got.Entries) != 2 {
		t.Fatalf("got %+v, %v", got, err)
	}
	first := got.Entries[0]
	if first.N != 1 || first.ID != "evening-mon" || first.Zone != "Europe/Berlin" || first.Profile != "supervisor" || first.Message != "Run the features gather." ||
		first.Problem != "" || first.Spec.Repeat == nil || first.Spec.Repeat.String() != "mon at 19:00" {
		t.Errorf("first entry %+v", first)
	}
	if second := got.Entries[1]; second.Zone != "" || second.Problem != "" || second.Spec.Repeat.String() != "every 2 hours" {
		t.Errorf("second entry %+v", second)
	}
	// An empty list is no schedules.
	if got, err := ParseSchedules([]byte(`{"schedules": []}`)); err != nil || len(got.Entries) != 0 {
		t.Errorf("an empty list: %+v %v", got, err)
	}
	// A file that is not the object is refused whole.
	for name, tc := range map[string]struct{ text, want string }{
		"no schedules":  {`{}`, "it has no schedules"},
		"another field": {`{"schedules": [], "zone": "UTC"}`, `the field "zone"`},
		"another case":  {`{"Schedules": []}`, `the field "Schedules"`},
		"twice":         {`{"schedules": [], "schedules": []}`, `names "schedules" twice`},
		"not an object": {`[]`, "no object"},
		"not a list":    {`{"schedules": {}}`, "no list"},
		"trailing text": {`{"schedules": []} {}`, "text follows"},
		"unended":       {`{"schedules": [`, "schedule 1"},
		"not UTF-8":     {"{\"schedules\": [\"\xff\"]}", "not UTF-8"},
	} {
		if _, err := ParseSchedules([]byte(tc.text)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	if _, err := ParseSchedules([]byte(`{"schedules": [], "x": "` + strings.Repeat("x", maxProfileSize) + `"}`)); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("a file past the bound: %v", err)
	}
}

func TestAScheduleThatBreaksARuleHasItsProblem(t *testing.T) {
	entry := func(fields string) string { return `{` + fields + `}` }
	good := `"id": "e", "when": "every day 19:00", "profile": "supervisor", "message": "m"`
	for name, tc := range map[string]struct{ entry, want string }{
		"not an object":   {`"evening"`, "schedule 1 is no object"},
		"another field":   {entry(good + `, "gather": {}`), `the field "gather"`},
		"another case":    {entry(`"ID": "e", "when": "every day 19:00", "profile": "supervisor", "message": "m"`), `the field "ID"`},
		"a key twice":     {entry(good + `, "message": "n"`), `names "message" twice`},
		"no id":           {entry(`"when": "every day 19:00", "profile": "supervisor", "message": "m"`), "has no id"},
		"a bad id":        {entry(`"id": "-e", "when": "every day 19:00", "profile": "supervisor", "message": "m"`), "id is not"},
		"a long id":       {entry(`"id": "` + strings.Repeat("e", 65) + `", "when": "every day 19:00", "profile": "supervisor", "message": "m"`), "id is not"},
		"no when":         {entry(`"id": "e", "profile": "supervisor", "message": "m"`), "has no when"},
		"a bad when":      {entry(`"id": "e", "when": "every 5m", "profile": "supervisor", "message": "m"`), "at most every 10 minutes"},
		"words after":     {entry(`"id": "e", "when": "every day 19:00 gather", "profile": "supervisor", "message": "m"`), "not part of a time"},
		"a bad zone":      {entry(good + `, "zone": "Mars/Olympus"`), "no time zone"},
		"no profile":      {entry(`"id": "e", "when": "every day 19:00", "message": "m"`), "names no profile"},
		"a bad profile":   {entry(`"id": "e", "when": "every day 19:00", "profile": "../x", "message": "m"`), "no profile's id"},
		"no action":       {entry(`"id": "e", "when": "every day 19:00", "profile": "supervisor"`), "has no action"},
		"an empty one":    {entry(`"id": "e", "when": "every day 19:00", "profile": "supervisor", "message": " "`), "say what the message is"},
		"a long message":  {entry(`"id": "e", "when": "every day 19:00", "profile": "supervisor", "message": "` + strings.Repeat("m", 4001) + `"`), "at most 4000"},
		"a number":        {entry(`"id": 7, "when": "every day 19:00", "profile": "supervisor", "message": "m"`), "is not text"},
		"null":            {entry(`"id": "e", "when": null, "profile": "supervisor", "message": "m"`), "is not text"},
		"an object value": {entry(`"id": "e", "when": "every day 19:00", "profile": "supervisor", "message": {"text": "m"}`), "is not text"},
	} {
		got, err := ParseSchedules([]byte(`{"schedules": [` + tc.entry + `, {` + strings.Replace(good, `"e"`, `"other"`, 1) + `}]}`))
		if err != nil || len(got.Entries) != 2 {
			t.Errorf("%s: %+v %v", name, got, err)
			continue
		}
		if !strings.Contains(got.Entries[0].Problem, tc.want) {
			t.Errorf("%s: the problem is %q, want %q", name, got.Entries[0].Problem, tc.want)
		}
		// The other entry goes out whatever this one says.
		if got.Entries[1].Problem != "" || got.Entries[1].ID != "other" {
			t.Errorf("%s: the good entry %+v", name, got.Entries[1])
		}
	}
	// Two entries with one id, in any case, both have a problem; a third
	// goes out.
	got, err := ParseSchedules([]byte(`{"schedules": [{` + good + `}, {` + strings.Replace(good, `"e"`, `"E"`, 1) + `}, {` + strings.Replace(good, `"e"`, `"f"`, 1) + `}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"schedules 1 and 2 share the id E", "schedules 1 and 2 share the id E", ""} {
		if got.Entries[i].Problem != want {
			t.Errorf("entry %d: %q, want %q", i+1, got.Entries[i].Problem, want)
		}
	}
}

// An entry that breaks a rule keeps its id, whatever field breaks it and
// wherever the id stands: pocketd keeps its record by it, and a twin of
// its id is seen (Codex review of #51, round 1).
func TestABrokenScheduleKeepsItsID(t *testing.T) {
	for name, entry := range map[string]string{
		"a field before the id": `{"gather": {}, "id": "evening", "when": "every day 19:00", "profile": "odrade", "message": "m"}`,
		"a field after the id":  `{"id": "evening", "gather": {}, "when": "every day 19:00", "profile": "odrade", "message": "m"}`,
		"a bad value first":     `{"when": 7, "id": "evening", "profile": "odrade", "message": "m"}`,
	} {
		got, err := ParseSchedules([]byte(`{"schedules": [` + entry + `, {"id": "EVENING", "when": "every day 8:00", "profile": "odrade", "message": "m"}]}`))
		if err != nil || len(got.Entries) != 2 {
			t.Fatalf("%s: %+v %v", name, got, err)
		}
		if got.Entries[0].ID != "evening" || got.Entries[0].Problem == "" {
			t.Errorf("%s: the broken entry %+v", name, got.Entries[0])
		}
		if !strings.Contains(got.Entries[1].Problem, "share the id") {
			t.Errorf("%s: its twin goes out: %+v", name, got.Entries[1])
		}
	}
	// An id named twice, or no text, is no id to keep a record by.
	for _, entry := range []string{
		`{"id": "a", "id": "b", "when": "every day 19:00", "profile": "odrade", "message": "m"}`,
		`{"id": 7, "when": "every day 19:00", "profile": "odrade", "message": "m"}`,
	} {
		got, err := ParseSchedules([]byte(`{"schedules": [` + entry + `]}`))
		if err != nil || got.Entries[0].ID != "" || got.Entries[0].Problem == "" {
			t.Errorf("%s: %+v %v", entry, got, err)
		}
	}
}

// No file is no schedules; a file in the folder is read only as a regular
// file of its own, as a profile is.
func TestLoadSchedules(t *testing.T) {
	dir := t.TempDir()
	if got, err := LoadSchedules(dir); err != nil || len(got.Entries) != 0 {
		t.Fatalf("no file: %+v, %v", got, err)
	}
	path := filepath.Join(dir, SchedulesFile)
	if err := os.WriteFile(path, []byte(`{"schedules": [{"id": "e", "when": "every day 19:00", "profile": "odrade", "message": "m"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := (Folder{Dir: dir}).Schedules(); err != nil || len(got.Entries) != 1 || got.Entries[0].Profile != "odrade" {
		t.Fatalf("the file: %+v, %v", got, err)
	}
	if err := os.WriteFile(path, []byte(`{"schedules": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSchedules(dir); err == nil || !strings.Contains(err.Error(), SchedulesFile) {
		t.Fatalf("an invalid file: %v", err)
	}
	// A link, to a file outside the protected folder, is no schedules file.
	outside := filepath.Join(t.TempDir(), "schedules.json")
	if err := os.WriteFile(outside, []byte(`{"schedules": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSchedules(dir); err == nil {
		t.Fatal("a link is read")
	}
	// A folder that is not there is an error, never a missing file.
	if _, err := LoadSchedules(filepath.Join(dir, "gone")); err == nil {
		t.Fatal("a missing folder reads as no schedules")
	}
}
