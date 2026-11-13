// Package indexer mirrors the org directory into SQLite. It only ever reads
// the org files.
package indexer

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ryansghost/journal-introspection/internal/store"
)

// Text files worth indexing; everything else (PDFs, images) is ignored.
var textExt = map[string]bool{
	".org": true, ".org_archive": true, ".md": true, ".txt": true, ".lisp": true, ".tex": true,
}

type Stats struct {
	At      time.Time
	Took    time.Duration
	Seen    int // files considered
	Changed int // re-parsed because the content changed
	Touched int // mtime changed but content did not
	Deleted int
	Err     string
}

type Indexer struct {
	db      *store.DB
	root    string
	exclude []string

	mu   sync.Mutex // one scan at a time
	last Stats
	// OnChange is called after a scan that changed journal entries.
	OnChange func()
}

func New(db *store.DB, root string, exclude []string) *Indexer {
	return &Indexer{db: db, root: root, exclude: exclude}
}

func (ix *Indexer) Last() Stats {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	return ix.last
}

// Run scans immediately and then every interval until ctx is cancelled.
func (ix *Indexer) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := ix.Scan(ctx); err != nil {
			slog.Error("scan failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Excluded reports whether a relative path matches an exclude glob. A pattern
// ending in /** matches a whole directory; otherwise it is matched against
// both the full relative path and the file name.
func Excluded(patterns []string, rel string) bool {
	for _, p := range patterns {
		if dir, ok := strings.CutSuffix(p, "/**"); ok {
			if rel == dir || strings.HasPrefix(rel, dir+"/") {
				return true
			}
			continue
		}
		if ok, _ := path.Match(p, rel); ok {
			return true
		}
		if ok, _ := path.Match(p, path.Base(rel)); ok {
			return true
		}
	}
	return false
}

type known struct {
	id          int64
	mtime, size int64
	sha         string
	deleted     bool
}

// Scan walks the tree once. A file is re-read only when its mtime or size
// differs from the stored row, and re-parsed only when its hash differs too.
func (ix *Indexer) Scan(ctx context.Context) (Stats, error) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	start := time.Now()
	st := Stats{At: start}

	have, err := ix.loadKnown(ctx)
	if err == nil {
		err = ix.walk(ctx, have, &st)
	}
	st.Took = time.Since(start)
	if err != nil {
		st.Err = err.Error()
	}
	ix.last = st
	if st.Changed+st.Touched+st.Deleted > 0 {
		slog.Info("scan", "seen", st.Seen, "changed", st.Changed, "touched", st.Touched, "deleted", st.Deleted, "took", st.Took.Round(time.Millisecond))
	}
	if err == nil && st.Changed+st.Deleted > 0 && ix.OnChange != nil {
		ix.OnChange()
	}
	return st, err
}

func (ix *Indexer) loadKnown(ctx context.Context) (map[string]known, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT id, path, mtime, size, sha256, deleted_at IS NOT NULL FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	have := map[string]known{}
	for rows.Next() {
		var k known
		var p string
		if err := rows.Scan(&k.id, &p, &k.mtime, &k.size, &k.sha, &k.deleted); err != nil {
			return nil, err
		}
		have[p] = k
	}
	return have, rows.Err()
}

func (ix *Indexer) walk(ctx context.Context, have map[string]known, st *Stats) error {
	seen := map[string]bool{}
	err := filepath.WalkDir(ix.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == ix.root {
				return err
			}
			slog.Warn("skip unreadable path", "path", p, "err", err)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, _ := filepath.Rel(ix.root, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		hidden := strings.HasPrefix(d.Name(), ".")
		if d.IsDir() {
			if hidden || Excluded(ix.exclude, rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if hidden || !d.Type().IsRegular() || !textExt[strings.ToLower(filepath.Ext(rel))] || Excluded(ix.exclude, rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		seen[rel] = true
		st.Seen++
		k, ok := have[rel]
		mtime, size := info.ModTime().UnixNano(), info.Size()
		if ok && !k.deleted && k.mtime == mtime && k.size == size {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			slog.Warn("skip unreadable file", "path", rel, "err", err)
			return nil
		}
		sum := sha256.Sum256(data)
		sha := hex.EncodeToString(sum[:])
		if ok && !k.deleted && k.sha == sha {
			st.Touched++
			_, err := ix.db.ExecContext(ctx, `UPDATE files SET mtime=?, size=? WHERE id=?`, mtime, size, k.id)
			return err
		}
		st.Changed++
		return ix.store(ctx, rel, string(data), mtime, size, sha)
	})
	if err != nil {
		return err
	}
	for p, k := range have {
		if seen[p] || k.deleted {
			continue
		}
		st.Deleted++
		if err := ix.remove(ctx, k.id); err != nil {
			return err
		}
	}
	return nil
}

func kindOf(rel string) string {
	if strings.HasPrefix(rel, "journal/") {
		if _, ok := JournalDate(path.Base(rel)); ok {
			return "journal"
		}
	}
	if strings.HasPrefix(rel, "roam/") {
		return "roam"
	}
	return "note"
}

var (
	titleKeyword = regexp.MustCompile(`(?im)^#\+title:\s*(.+)$`)
	firstHeading = regexp.MustCompile(`(?m)^\*+\s+(.+)$`)
)

func titleOf(rel, src string) string {
	if m := titleKeyword.FindStringSubmatch(src); m != nil {
		return strings.TrimSpace(m[1])
	}
	if strings.HasPrefix(path.Ext(rel), ".org") {
		if m := firstHeading.FindStringSubmatch(src); m != nil {
			return strings.TrimSpace(m[1])
		}
	}
	return path.Base(rel)
}

func (ix *Indexer) store(ctx context.Context, rel, src string, mtime, size int64, sha string) error {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	kind, title, now := kindOf(rel), titleOf(rel, src), time.Now().Unix()
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO files(path, kind, title, mtime, size, sha256, indexed_at) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(path) DO UPDATE SET kind=excluded.kind, title=excluded.title, mtime=excluded.mtime,
			size=excluded.size, sha256=excluded.sha256, indexed_at=excluded.indexed_at, deleted_at=NULL
		RETURNING id`, rel, kind, title, mtime, size, sha, now).Scan(&id)
	if err != nil {
		return err
	}
	if err := clearDerived(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO search(rowid, path, title, body) VALUES(?,?,?,?)`, id, rel, title, src); err != nil {
		return err
	}
	if kind == "journal" {
		date, _ := JournalDate(path.Base(rel))
		for i, e := range SplitJournal(date, src) {
			props, _ := json.Marshal(e.Props)
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO entries(file_id, date, time, heading, body_org, tags, props_json, word_count, content_hash, seq)
				VALUES(?,?,?,?,?,?,?,?,?,?)`,
				id, e.Date, e.Time, e.Heading, e.Body, strings.Join(e.Tags, " "), string(props), e.Words, e.Hash, i); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func clearDerived(ctx context.Context, tx *sql.Tx, id int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM entries WHERE file_id=?`, id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM search WHERE rowid=?`, id)
	return err
}

// remove marks a vanished file as deleted. Its grades stay, keyed by entry text.
func (ix *Indexer) remove(ctx context.Context, id int64) error {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := clearDerived(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE files SET deleted_at=? WHERE id=?`, time.Now().Unix(), id); err != nil {
		return err
	}
	return tx.Commit()
}
