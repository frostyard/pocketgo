package state

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
)

func foldingRows() []Row {
	return BuildRows([]proto.Entry{
		{ID: 1, Kind: proto.EntryUser, Text: "[T300 · Murbella · done] typed by me"},
		{ID: 2, Kind: proto.EntryUser, Text: "[T300 · Murbella · done] Merged frostyard/pocketgo#151.", Sender: &proto.Sender{Kind: proto.SenderService}},
		{ID: 3, Kind: proto.EntryUser, Text: "A supervisor's message", Sender: &proto.Sender{Kind: proto.SenderSupervisor, Session: 7}},
	}, proto.Live{}, nil, nil)
}

func TestOnlyServiceMessagesStartFolded(t *testing.T) {
	rows := foldingRows()
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, unfolded := range []Unfolded{nil, {}} {
		for i, row := range rows {
			want := i == 1
			if row.FromService() != want || unfolded.Folds(row) != want {
				t.Errorf("row %s: FromService = %v, Folds = %v; want %v", row.Key, row.FromService(), unfolded.Folds(row), want)
			}
		}
	}
	for _, kind := range []RowKind{RowAssistant, RowTool, RowError, RowNote, RowWorking, RowCompaction, RowQuestion} {
		row := Row{Kind: kind, Sender: &proto.Sender{Kind: proto.SenderService}}
		if row.FromService() || (Unfolded{}).Folds(row) {
			t.Errorf("non-user kind %v folds", kind)
		}
	}
}

func TestAQueuedServiceMessageStartsFolded(t *testing.T) {
	rows := BuildRows(nil, proto.Live{Queued: []proto.QueuedMessage{{SubmissionID: 9, Text: "[scheduled] Morning brief.", Sender: &proto.Sender{Kind: proto.SenderService}}}}, nil, nil)
	if len(rows) != 1 || !rows[0].Queued || !rows[0].FromService() || !(Unfolded{}).Folds(rows[0]) || !(Unfolded(nil)).Folds(rows[0]) {
		t.Fatalf("queued rows = %+v", rows)
	}
}

func TestTogglingKeepsOnlyOpenServiceRows(t *testing.T) {
	rows := foldingRows()
	u := Unfolded{}
	u.Toggle(rows[1])
	if u.Folds(rows[1]) || !u["e2"] || len(u) != 1 {
		t.Fatalf("opened row: %+v", u)
	}
	u.Toggle(rows[1])
	if !u.Folds(rows[1]) || len(u) != 0 {
		t.Fatalf("folded row: %+v", u)
	}
	for _, row := range []Row{rows[0], rows[2]} {
		u.Toggle(row)
		if len(u) != 0 || u.Folds(row) {
			t.Errorf("toggle on %s: %+v", row.Key, u)
		}
	}
}

func TestFoldOfFindsTheHeaderAndFirstPlainLine(t *testing.T) {
	head := "[T300 · Murbella · done]"
	for _, tc := range []struct {
		name, text string
		want       Folded
	}{
		{"empty", "", Folded{}},
		{"header alone", head, Folded{Head: head}},
		{"empty first line", head + "\n\nMerged #151.", Folded{head, "Merged #151."}},
		{"leading space and bold", "  " + head + "   \n  \n**Merged** #151.", Folded{head, "Merged #151."}},
		{"inline markdown", head + " **PR #151** is `ready`: see [the PR](https://github.com/frostyard/pocketgo/pull/151) and ~~old~~ ![shot](a.png)", Folded{head, "PR #151 is ready: see the PR and old shot"}},
		{"heading", head + " ## Summary\n- one", Folded{head, "Summary"}},
		{"quote", head + " > quoted\ttext", Folded{head, "quoted text"}},
		{"single marks survive", head + " keep snake_case and 2*3", Folded{head, "keep snake_case and 2*3"}},
		{"link is not header", "[the PR](https://x) merged", Folded{"", "the PR merged"}},
		{"unclosed header", "[unclosed header\nnext", Folded{"", "[unclosed header"}},
		{"plain brief", "Odrade, the team's supervisor, delegated this to you in thread T7: X\n\nBody", Folded{"", "Odrade, the team's supervisor, delegated this to you in thread T7: X"}},
		{"nested bracket is not header", "[outer [inner] words", Folded{"", "[outer [inner] words"}},
		{"closing bracket on next line is not header", "[header\n] words", Folded{"", "[header"}},
		{"skip marks only", head + " **__~~`\n > \n+ words", Folded{head, "words"}},
		{"minus list", "- words", Folded{"", "words"}},
		{"star list", "* words", Folded{"", "words"}},
		{"underline emphasis and whitespace", "__one__ \t two\r three", Folded{"", "one two three"}},
		{"quote without space", ">words", Folded{"", "words"}},
		{"hash without space stays", "#tag", Folded{"", "#tag"}},
		{"header at limit", "[" + strings.Repeat("é", FoldedRunes-2) + "] words", Folded{"[" + strings.Repeat("é", FoldedRunes-2) + "]", "words"}},
		{"header over limit", "[" + strings.Repeat("x", FoldedRunes-1) + "]", Folded{"", "[" + strings.Repeat("x", FoldedRunes-2) + "…"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FoldOf(tc.text); got != tc.want {
				t.Errorf("FoldOf(%q) = %+v, want %+v", tc.text, got, tc.want)
			}
		})
	}
}

func TestFoldOfCutsByRunesIncludingTheEllipsis(t *testing.T) {
	for _, n := range []int{FoldedRunes, FoldedRunes + 1, 300} {
		text := strings.Repeat("é", n)
		want := text
		if n > FoldedRunes {
			want = strings.Repeat("é", FoldedRunes-1) + "…"
		}
		got := FoldOf("[T1 · a · done] " + text)
		if got != (Folded{Head: "[T1 · a · done]", Line: want}) || utf8.RuneCountInString(got.Line) != FoldedRunes {
			t.Errorf("%d runes: %+v, want %q (%d runes)", n, got, want, FoldedRunes)
		}
	}
}
