package files

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestRootsExcludeBothSidesOfRefusedPaths covers lexical and resolved comparisons.
func TestRootsExcludeBothSidesOfRefusedPaths(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "data")
	inside := filepath.Join(data, "inside")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "linked")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatal(err)
	}
	refusedLink := filepath.Join(base, "data-link")
	if err := os.Symlink(data, refusedLink); err != nil {
		t.Fatal(err)
	}
	ordinary := filepath.Join(base, "ordinary")
	absent := filepath.Join(base, "missing")
	homes := []Home{{ID: "equal", Path: data}, {ID: "parent", Path: base}, {ID: "inside", Path: inside}, {ID: "link", Path: link}, {ID: "relative", Path: "relative"}, {ID: "empty"}, {ID: "z", Name: "Z", Path: ordinary, Private: true}, {ID: "a", Path: absent}, {ID: "prefix", Path: data + "-other"}}
	want := []Home{{ID: "a", Path: absent}, {ID: "prefix", Path: data + "-other"}, {ID: "z", Name: "Z", Path: ordinary, Private: true}}
	for _, refused := range []string{data, refusedLink} {
		if got := Roots(homes, []string{"", refused}); !reflect.DeepEqual(got, want) {
			t.Fatalf("refused %s: %+v", refused, got)
		}
	}
	if got := Roots(homes, []string{string(filepath.Separator)}); len(got) != 0 {
		t.Fatal(got)
	}
	if got := Roots(nil, nil); got == nil {
		t.Fatal("nil roots")
	}
}
