package storetest

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/frostyard/pocketgo/service/internal/store"
)

type schemaRow struct {
	Type, Name, Table string
	SQL               sql.NullString
}

func readSchema(t *testing.T, path string) ([]schemaRow, int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var schema []schemaRow
	for rows.Next() {
		var row schemaRow
		if err := rows.Scan(&row.Type, &row.Name, &row.Table, &row.SQL); err != nil {
			t.Fatal(err)
		}
		schema = append(schema, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return schema, version
}

func TestTemplateMatchesAFreshStore(t *testing.T) {
	freshPath := filepath.Join(t.TempDir(), "fresh.sqlite")
	fresh, err := store.Open(context.Background(), freshPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "copy.sqlite")
	copied := Open(t, copyPath, nil)
	if err := copied.Close(); err != nil {
		t.Fatal(err)
	}
	freshSchema, freshVersion := readSchema(t, freshPath)
	copySchema, copyVersion := readSchema(t, copyPath)
	if !reflect.DeepEqual(freshSchema, copySchema) {
		t.Fatalf("template schema differs from fresh schema:\n%+v\n%+v", copySchema, freshSchema)
	}
	if freshVersion != store.SchemaVersion || copyVersion != freshVersion {
		t.Fatalf("versions: fresh=%d copy=%d want=%d", freshVersion, copyVersion, store.SchemaVersion)
	}
	// Check the template before Open could repair a missing migration, too.
	rawPath := filepath.Join(t.TempDir(), "raw.sqlite")
	if err := writeTemplate(rawPath); err != nil {
		t.Fatal(err)
	}
	rawSchema, rawVersion := readSchema(t, rawPath)
	if !reflect.DeepEqual(rawSchema, freshSchema) || rawVersion != freshVersion {
		t.Fatal("unopened template differs from a fresh store")
	}
}

func TestWriteTemplateRefusesAnExistingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.sqlite")
	original := []byte("do not replace")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeTemplate(path); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing path: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatal("existing file was changed")
	}
}

func TestWriteTemplateCreatesAPrivateIndependentCopy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copy.sqlite")
	if err := writeTemplate(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("database permissions: %v", info.Mode())
	}
	data, err := Template()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	otherPath := filepath.Join(t.TempDir(), "other.sqlite")
	if err := writeTemplate(otherPath); err != nil {
		t.Fatal(err)
	}
	other, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(other) != string(data) {
		t.Fatal("changing one copy changed another")
	}
}
