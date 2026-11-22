package pig

import (
	"context"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/api"
)

// EntriesAfter reads a session's entries after one, oldest first, at most
// as many as asked, as the search index reads them (ADR-0050).
func TestEntriesAfterReadsOldestFirstUpToMost(t *testing.T) {
	f := newFixture(t)
	e := f.open(fauxModels(answer("one"), answer("two")))
	defer func() { _ = e.Close(context.Background()) }()
	ctx := context.Background()
	id, _ := e.CreateSession(ctx, t.Name(), api.SessionSpec{Cwd: f.work, Model: fauxRef})
	if err := e.Watch(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"first", "second"} {
		if _, err := e.Send(ctx, id, "u:dev:"+text, text); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, e, id)
	}
	all, _, err := e.Transcript(ctx, id)
	if err != nil || len(all) < 4 {
		t.Fatalf("transcript %+v, %v", all, err)
	}
	got, err := e.EntriesAfter(ctx, id, 0, 100)
	if err != nil || len(got) != len(all) {
		t.Fatalf("EntriesAfter(0) = %d entries, %v; want %d", len(got), err, len(all))
	}
	for i := range got {
		if got[i].ID != all[i].ID || got[i].Text != all[i].Text || got[i].Kind != all[i].Kind {
			t.Errorf("entry %d: %+v, want %+v", i, got[i], all[i])
		}
	}
	if got[0].Kind != proto.EntryUser || got[0].Text != "first" {
		t.Errorf("the first entry %+v", got[0])
	}
	page, err := e.EntriesAfter(ctx, id, all[0].ID, 3)
	if err != nil || len(page) != 3 || page[0].ID != all[1].ID || page[2].ID != all[3].ID {
		t.Fatalf("EntriesAfter(first, 3) = %+v, %v", page, err)
	}
	if none, err := e.EntriesAfter(ctx, id, all[len(all)-1].ID, 3); err != nil || len(none) != 0 {
		t.Fatalf("after the last: %+v, %v", none, err)
	}
}
