package store

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/search"
)

var searchAt = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

func doc(entry int64, text string) search.Doc {
	return search.Doc{Entry: entry, Field: proto.SearchUser, At: searchAt.Add(time.Duration(entry) * time.Second), Text: text}
}

func words(t *testing.T, q string) string {
	t.Helper()
	w, err := proto.SearchWords(q)
	if err != nil {
		t.Fatal(err)
	}
	return search.Match(w)
}

// The index keeps each entry once with its session's cursor, which never
// moves back; it finds every word as the start of a word, any case.
func TestTheIndexKeepsEntriesOnceWithTheirCursor(t *testing.T) {
	s, _ := open(t)
	if err := s.IndexEntries(ctx, 1, 4, []search.Doc{doc(1, "The arm64 job fails"), doc(3, "Pin the RUNNER to ubuntu-24.04")}); err != nil {
		t.Fatal(err)
	}
	// Run again, as after a failure that kept the transaction: nothing
	// twice, and a cursor behind stays where it is.
	if err := s.IndexEntries(ctx, 1, 2, []search.Doc{doc(1, "The arm64 job fails"), doc(3, "again")}); err != nil {
		t.Fatal(err)
	}
	if err := s.IndexEntries(ctx, 2, 9, []search.Doc{{Entry: 9, Field: proto.SearchAssistant, Text: "Tools: team_delegate"}}); err != nil {
		t.Fatal(err)
	}
	cursors, err := s.SearchCursors(ctx)
	if err != nil || len(cursors) != 2 || cursors[1] != 4 || cursors[2] != 9 {
		t.Fatalf("cursors %v, %v", cursors, err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM search_entries`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("%d rows, %v", n, err)
	}
	for _, tc := range []struct {
		q    string
		want []int64
	}{
		{"arm64", []int64{1}},
		{"runner UBUNTU", []int64{3}},
		{"run", []int64{3}},
		{"unner", nil},
		{"24", []int64{3}},
		{"delegate", []int64{9}},
		{"the", []int64{3, 1}},
		{"again", nil},
	} {
		hits, cut, err := s.SearchEntries(ctx, words(t, tc.q), proto.MaxSearchSessionHits, 10)
		if err != nil {
			t.Fatalf("%q: %v", tc.q, err)
		}
		var got []int64
		for _, h := range hits {
			got = append(got, h.Entry)
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) || cut {
			t.Errorf("%q found %v (cut %v), want %v", tc.q, got, cut, tc.want)
		}
	}
	hits, _, err := s.SearchEntries(ctx, words(t, "arm64"), 5, 10)
	if err != nil || len(hits) != 1 || hits[0].Session != 1 || hits[0].Field != proto.SearchUser || !hits[0].At.Equal(searchAt.Add(time.Second)) ||
		hits[0].Text != "The arm64 job fails" {
		t.Fatalf("hit %+v, %v", hits, err)
	}
	hits, _, err = s.SearchEntries(ctx, words(t, "delegate"), 5, 10)
	if err != nil || len(hits) != 1 || !hits[0].At.IsZero() || hits[0].Field != proto.SearchAssistant {
		t.Fatalf("a hit with no time: %+v, %v", hits, err)
	}
}

// At most perSession hits of one session and limit in all, the newest
// first, and cut says the index holds more.
func TestSearchEntriesKeepsItsLimits(t *testing.T) {
	s, _ := open(t)
	var long []search.Doc
	for id := int64(1); id <= 8; id++ {
		long = append(long, doc(id, "deploy step"))
	}
	if err := s.IndexEntries(ctx, 1, 8, long); err != nil {
		t.Fatal(err)
	}
	if err := s.IndexEntries(ctx, 2, 20, []search.Doc{doc(10, "deploy"), doc(20, "deployed")}); err != nil {
		t.Fatal(err)
	}
	hits, cut, err := s.SearchEntries(ctx, words(t, "deploy"), 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range hits {
		got = append(got, fmt.Sprintf("%d/%d", h.Session, h.Entry))
	}
	if want := "[2/20 2/10 1/8 1/7 1/6]"; fmt.Sprint(got) != want || !cut {
		t.Errorf("found %v cut %v, want %s cut", got, cut, want)
	}
	hits, cut, err = s.SearchEntries(ctx, words(t, "deploy"), 10, 4)
	if err != nil || len(hits) != 4 || !cut || hits[0].Entry != 20 {
		t.Errorf("limit 4: %+v cut %v, %v", hits, cut, err)
	}
	hits, cut, err = s.SearchEntries(ctx, words(t, "deployed"), 10, 4)
	if err != nil || len(hits) != 1 || cut {
		t.Errorf("one hit: %+v cut %v, %v", hits, cut, err)
	}
}

// A work database at version 22 gets the index, empty, so the first start
// reads every transcript into it.
func TestTheSearchMigrationUpgradesADatabaseAt22(t *testing.T) {
	if SchemaVersion < 23 || !strings.Contains(migrations[22], "search_entries") || !strings.Contains(migrations[21], "questions") {
		t.Fatalf("schema version %d: search is not migration 23", SchemaVersion)
	}
	path := filepath.Join(t.TempDir(), "work.sqlite")
	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	steps := []string{`CREATE TABLE schema_version (version INTEGER NOT NULL)`}
	steps = append(steps, migrations[:22]...)
	steps = append(steps, `INSERT INTO schema_version (version) VALUES (22)`,
		`INSERT INTO sessions (id, title, cwd, provider, model, created_by, created_at, updated_at) VALUES (1, 'Old', '/w', 'p', 'm', 'd', 1, 1)`)
	for i, step := range steps {
		if _, err := db.Exec(step); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	_ = db.Close()
	s, err := Open(ctx, path, func() time.Time { return searchAt })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if cursors, err := s.SearchCursors(ctx); err != nil || len(cursors) != 0 {
		t.Fatalf("cursors %v, %v", cursors, err)
	}
	if err := s.IndexEntries(ctx, 1, 1, []search.Doc{doc(1, "an old reply")}); err != nil {
		t.Fatal(err)
	}
	if hits, _, err := s.SearchEntries(ctx, words(t, "old"), 5, 10); err != nil || len(hits) != 1 {
		t.Fatalf("hits %+v, %v", hits, err)
	}
}

// vocabulary is the words of a synthetic transcript: common English words
// and a long tail.
var vocabulary = func() []string {
	common := strings.Fields("the a to of and in is it that for on with this be as not are run test fix job build deploy merge review " +
		"branch commit pull request session agent thread report change file code error check pass fail ci arm64 runner image")
	out := common
	for i := 0; len(out) < 20000; i++ {
		out = append(out, fmt.Sprintf("w%dx%d", i%977, i))
	}
	return out
}()

// fillIndex keeps sessions × perSession synthetic entries of about 60
// words each, as replies and messages run.
func fillIndex(tb testing.TB, s *Store, sessions, perSession int) {
	tb.Helper()
	r := rand.New(rand.NewPCG(1, 2))
	id := int64(0)
	for sess := int64(1); sess <= int64(sessions); sess++ {
		docs := make([]search.Doc, 0, perSession)
		for range perSession {
			id++
			var b strings.Builder
			for range 60 {
				// Zipf-like: the common words most of the time.
				w := vocabulary[r.IntN(40)]
				if r.IntN(3) == 0 {
					w = vocabulary[r.IntN(len(vocabulary))]
				}
				b.WriteString(w + " ")
			}
			docs = append(docs, search.Doc{Entry: id, Field: proto.SearchAssistant, At: searchAt.Add(time.Duration(id) * time.Second), Text: b.String()})
		}
		if err := s.IndexEntries(ctx, sess, id, docs); err != nil {
			tb.Fatal(err)
		}
	}
	// A word said once, in an old session's reply.
	id++
	if err := s.IndexEntries(ctx, 3, id, []search.Doc{{Entry: id, Field: proto.SearchAssistant, Text: "the flux capacitor drifted"}}); err != nil {
		tb.Fatal(err)
	}
}

// BenchmarkSearchEntries searches an index of 400 sessions of 500 entries
// each, twice today's 190 sessions: a word said once, a common word, and a
// one-letter prefix that matches nearly every entry.
func BenchmarkSearchEntries(b *testing.B) {
	path := filepath.Join(b.TempDir(), "work.sqlite")
	s, err := Open(ctx, path, nil)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	fillIndex(b, s, 400, 500)
	for _, q := range []string{"capacitor", "deploy", "a", "arm64 runner"} {
		w, err := proto.SearchWords(q)
		if err != nil {
			b.Fatal(err)
		}
		match := search.Match(w)
		b.Run(strings.ReplaceAll(q, " ", "_"), func(b *testing.B) {
			for b.Loop() {
				if _, _, err := s.SearchEntries(ctx, match, proto.MaxSearchSessionHits, proto.DefaultSearchLimit); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
