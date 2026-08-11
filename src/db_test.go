package autojournal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIndexDSNEscapesReservedCharacters(t *testing.T) {
	// '?' and '#' are legal path bytes on every Unix filesystem and both
	// terminate a SQLite URI: without escaping, the database silently lands
	// at a truncated path with none of the configured pragmas.
	dir := filepath.Join(t.TempDir(), "we?ird#state")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "index?v=1#frag.sqlite")
	db, err := openSQLite(path)
	if err != nil {
		t.Fatalf("openSQLite: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO t (v) VALUES ('round-trip')`); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := db.QueryRow(`SELECT v FROM t`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "round-trip" {
		t.Errorf("v = %q", got)
	}
	// The database exists at the literal path, not a truncated one.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("no database at the literal path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "index")); err == nil {
		t.Error("a truncated-path database exists")
	}

	// '%' is the third reserved byte: an unescaped '%XX' sequence is
	// percent-decoded by the URI parser, so the database silently lands at
	// the decoded path. systemd-escaped directories make literal '%XX'
	// sequences a real occurrence in owner paths.
	pdir := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(pdir, 0o700); err != nil {
		t.Fatal(err)
	}
	ppath := filepath.Join(pdir, "ind%65x.sqlite")
	pdb, err := openSQLite(ppath)
	if err != nil {
		t.Fatalf("openSQLite: %v", err)
	}
	defer pdb.Close()
	if _, err := pdb.Exec(`CREATE TABLE t (v TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ppath); err != nil {
		t.Errorf("no database at the literal path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pdir, "index.sqlite")); err == nil {
		t.Error("a database exists at the percent-decoded path")
	}
}
