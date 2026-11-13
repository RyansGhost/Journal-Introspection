package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ryansghost/journal-introspection/internal/store"
)

func TestSplitJournal(t *testing.T) {
	src, err := os.ReadFile("../../testdata/org/journal/20261113.org")
	if err != nil {
		t.Fatal(err)
	}
	got := SplitJournal("2026-11-13", string(src))
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d", len(got))
	}
	if got[0].Time != "07:37" || got[0].Heading != "" || got[0].Words == 0 {
		t.Errorf("first entry: %+v", got[0])
	}
	e := got[1]
	if e.Time != "21:10" || e.Heading != "Evening notes" {
		t.Errorf("second entry time/heading: %q %q", e.Time, e.Heading)
	}
	if len(e.Tags) != 2 || e.Tags[0] != "phone" || e.Tags[1] != "voice" {
		t.Errorf("tags: %v", e.Tags)
	}
	if e.Props["SOURCE"] != "voice-memo" {
		t.Errorf("props: %v", e.Props)
	}
	if want := "Tired after"; e.Body[:len(want)] != want {
		t.Errorf("property drawer leaked into body: %q", e.Body[:40])
	}

	// No time headings: the whole file is one entry, keywords dropped.
	one := SplitJournal("2026-11-12", "#+title: Thursday\n\nSlept badly.\n")
	if len(one) != 1 || one[0].Time != "" || one[0].Body != "Slept badly." {
		t.Errorf("untimed file: %+v", one)
	}
	// Single-digit hours are padded so entries sort.
	if e := SplitJournal("2026-11-10", "** 6:05\nUp early.")[0]; e.Time != "06:05" {
		t.Errorf("time not padded: %q", e.Time)
	}
}

func TestExcluded(t *testing.T) {
	pats := []string{"crypto.org", "finance*.org", "invoices/**"}
	for rel, want := range map[string]bool{
		"crypto.org": true, "finance-learning.org": true, "invoices/2026/a.org": true,
		"invoices": true, "projects/finance.org": true, "life.org": false, "journal/20260101.org": false,
	} {
		if got := Excluded(pats, rel); got != want {
			t.Errorf("Excluded(%q) = %v, want %v", rel, got, want)
		}
	}
}

func copyTree(t *testing.T, dst string) {
	t.Helper()
	if err := os.CopyFS(dst, os.DirFS("../../testdata/org")); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, db *store.DB, q string, args ...any) (n int) {
	t.Helper()
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestScanChangeDetection(t *testing.T) {
	root := t.TempDir()
	copyTree(t, root)
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ix := New(db, root, []string{"crypto.org"})
	ctx := context.Background()

	st, err := ix.Scan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Changed != 7 {
		t.Errorf("first scan: want 7 files read, got %d", st.Changed)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM files WHERE path='crypto.org'`); n != 0 {
		t.Error("excluded file was indexed")
	}
	if n := count(t, db, `SELECT COUNT(*) FROM entries`); n != 4 {
		t.Errorf("want 4 journal entries, got %d", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM files WHERE kind='journal'`); n != 3 {
		t.Errorf("want 3 journal files (not-a-day.org is a note), got %d", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM search WHERE search MATCH '"zettelkasten"'`); n != 1 {
		t.Errorf("full-text search: got %d hits", n)
	}

	// Nothing changed: nothing is read.
	if st, _ = ix.Scan(ctx); st.Changed+st.Touched+st.Deleted != 0 {
		t.Errorf("idle scan did work: %+v", st)
	}

	// Touched, same content: noted, not re-parsed.
	day := filepath.Join(root, "journal", "20261113.org")
	later := time.Now().Add(time.Hour)
	os.Chtimes(day, later, later)
	if st, _ = ix.Scan(ctx); st.Touched != 1 || st.Changed != 0 {
		t.Errorf("touch: %+v", st)
	}

	// One entry edited: the other entry keeps its hash, so it keeps its grades.
	var keep string
	db.QueryRow(`SELECT content_hash FROM entries WHERE date='2026-11-13' AND time='07:37'`).Scan(&keep)
	src, _ := os.ReadFile(day)
	os.WriteFile(day, append(src, []byte("\nOne more line tonight.\n")...), 0o644)
	if st, _ = ix.Scan(ctx); st.Changed != 1 {
		t.Errorf("edit: %+v", st)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM entries WHERE content_hash=?`, keep); n != 1 {
		t.Error("the unedited entry's hash changed")
	}
	if n := count(t, db, `SELECT COUNT(*) FROM entries WHERE date='2026-11-13'`); n != 2 {
		t.Errorf("entries duplicated on re-index: %d", n)
	}

	// Deleted file: entries and search rows go, the row is kept as deleted.
	os.Remove(day)
	if st, _ = ix.Scan(ctx); st.Deleted != 1 {
		t.Errorf("delete: %+v", st)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM entries WHERE date='2026-11-13'`); n != 0 {
		t.Error("entries survived file deletion")
	}
	// And it comes back if the file does.
	os.WriteFile(day, src, 0o644)
	if st, _ = ix.Scan(ctx); st.Changed != 1 {
		t.Errorf("restore: %+v", st)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM files WHERE path='journal/20261113.org' AND deleted_at IS NULL`); n != 1 {
		t.Error("restored file still marked deleted")
	}
}
