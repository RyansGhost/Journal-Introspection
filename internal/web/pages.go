package web

import (
	"context"
	"database/sql"
	"errors"
	"html"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/ryansghost/journal-introspection/internal/indexer"
	"github.com/ryansghost/journal-introspection/internal/orgrender"
)

type crumb struct{ Name, URL string }

func crumbs(rel string) []crumb {
	out := []crumb{{"org", "/files/"}}
	if rel == "" {
		return out
	}
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		out = append(out, crumb{p, "/files/" + strings.Join(parts[:i+1], "/")})
	}
	return out
}

type listing struct {
	Name, URL, Title string
	Dir              bool
	Modified         string
}

// files serves both directory listings and single files. Both come from the
// index, so excluded files do not exist as far as this handler is concerned.
func (s *Server) files(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rel := strings.Trim(path.Clean("/"+r.PathValue("path")), "/")

	var kind, title string
	var mtime int64
	err := s.db.QueryRowContext(ctx, `SELECT kind, title, mtime FROM files WHERE path=? AND deleted_at IS NULL`, rel).Scan(&kind, &title, &mtime)
	if err == nil {
		s.file(w, r, rel, title, mtime)
		return
	}
	if !errors.Is(err, sql.ErrNoRows) {
		s.fail(w, err)
		return
	}

	prefix := ""
	if rel != "" {
		prefix = rel + "/"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT path, title, mtime FROM files WHERE deleted_at IS NULL AND substr(path, 1, ?) = ? ORDER BY path`, len(prefix), prefix)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer rows.Close()
	var dirs, items []listing
	seen := map[string]bool{}
	for rows.Next() {
		var p, t string
		var m int64
		if err := rows.Scan(&p, &t, &m); err != nil {
			s.fail(w, err)
			return
		}
		rest := p[len(prefix):]
		if name, _, nested := strings.Cut(rest, "/"); nested {
			if !seen[name] {
				seen[name] = true
				dirs = append(dirs, listing{Name: name + "/", URL: "/files/" + prefix + name, Dir: true})
			}
			continue
		}
		if t == rest {
			t = ""
		}
		items = append(items, listing{Name: rest, URL: "/files/" + p, Title: t, Modified: time.Unix(0, m).Format(dateFmt)})
	}
	if len(dirs)+len(items) == 0 && rel != "" {
		http.NotFound(w, r)
		return
	}
	// Journal days read best newest first.
	if rel == "journal" {
		sort.Slice(items, func(i, j int) bool { return items[i].Name > items[j].Name })
	}
	s.render(w, "files.html", map[string]any{"Title": "/" + rel, "Crumbs": crumbs(rel), "Items": append(dirs, items...), "Count": len(items)})
}

// resolver turns links between org files into links inside the app: file
// links relative to the directory of the file being shown, and org-roam id
// links through the index. Links to files that are not indexed are left alone.
func (s *Server) resolver(ctx context.Context, dir string) orgrender.Resolver {
	return func(protocol, target string) string {
		var rel string
		switch protocol {
		case "id":
			s.db.QueryRowContext(ctx, `SELECT f.path FROM org_ids o JOIN files f ON f.id = o.file_id WHERE o.org_id=? AND f.deleted_at IS NULL`, target).Scan(&rel)
		case "file", "":
			target, _, _ = strings.Cut(target, "::")
			if target == "" || strings.HasPrefix(target, "/") || strings.HasPrefix(target, "~") {
				return ""
			}
			s.db.QueryRowContext(ctx, `SELECT path FROM files WHERE path=? AND deleted_at IS NULL`, path.Join(dir, target)).Scan(&rel)
		}
		if rel == "" {
			return ""
		}
		return (&url.URL{Path: "/files/" + rel}).String()
	}
}

const maxRender = 4 << 20

func (s *Server) file(w http.ResponseWriter, r *http.Request, rel, title string, mtime int64) {
	f, err := s.root.Open(rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxRender))
	if err != nil {
		s.fail(w, err)
		return
	}
	var body template.HTML
	if strings.HasPrefix(path.Ext(rel), ".org") {
		body = orgrender.HTML(string(raw), true, s.resolver(r.Context(), path.Dir(rel)))
	} else {
		body = template.HTML(`<pre class="plain">` + html.EscapeString(string(raw)) + `</pre>`)
	}
	data := map[string]any{"Title": title, "Crumbs": crumbs(rel), "Body": body, "Modified": time.Unix(0, mtime).Format("2 Jan 2006 15:04")}
	if date, ok := indexer.JournalDate(path.Base(rel)); ok && strings.HasPrefix(rel, "journal/") {
		data["JournalURL"] = "/journal/" + date
	}
	s.render(w, "file.html", data)
}

type hit struct {
	URL, Title, Path string
	Snippet          template.HTML
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	data := map[string]any{"Title": "Search", "Q": q}
	if q != "" {
		// Quote every word so user input can never be FTS5 syntax.
		var terms []string
		for _, f := range strings.Fields(q) {
			terms = append(terms, `"`+strings.ReplaceAll(f, `"`, `""`)+`"`)
		}
		rows, err := s.db.QueryContext(r.Context(), `
			SELECT f.path, f.title, snippet(search, 2, char(1), char(2), '…', 28)
			FROM search JOIN files f ON f.id = search.rowid
			WHERE search MATCH ? AND f.deleted_at IS NULL ORDER BY rank LIMIT 60`, strings.Join(terms, " "))
		if err != nil {
			s.fail(w, err)
			return
		}
		defer rows.Close()
		var hits []hit
		for rows.Next() {
			var h hit
			var snip string
			if err := rows.Scan(&h.Path, &h.Title, &snip); err != nil {
				s.fail(w, err)
				return
			}
			snip = html.EscapeString(snip)
			snip = strings.NewReplacer("\x01", "<mark>", "\x02", "</mark>").Replace(snip)
			h.Snippet = template.HTML(snip)
			h.URL = "/files/" + h.Path
			if date, ok := indexer.JournalDate(path.Base(h.Path)); ok && strings.HasPrefix(h.Path, "journal/") {
				h.URL = "/journal/" + date
			}
			hits = append(hits, h)
		}
		data["Hits"] = hits
	}
	s.render(w, "search.html", data)
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	type kindCount struct {
		Kind string
		N    int
	}
	var kinds []kindCount
	rows, err := s.db.QueryContext(ctx, `SELECT kind, COUNT(*) FROM files WHERE deleted_at IS NULL GROUP BY kind ORDER BY kind`)
	if err != nil {
		s.fail(w, err)
		return
	}
	for rows.Next() {
		var k kindCount
		rows.Scan(&k.Kind, &k.N)
		kinds = append(kinds, k)
	}
	rows.Close()

	type failure struct {
		Date, Error string
		Attempts    int
	}
	var failures []failure
	rows, err = s.db.QueryContext(ctx, `
		SELECT COALESCE(MIN(e.date), '?'), f.attempts, f.last_error FROM grade_failures f
		LEFT JOIN entries e ON e.content_hash = f.entry_hash
		WHERE f.rubric_version = ? AND f.model = ? GROUP BY f.entry_hash ORDER BY 1 DESC LIMIT 20`,
		s.grader.Rubric.Version, s.grader.Model)
	if err != nil {
		s.fail(w, err)
		return
	}
	for rows.Next() {
		var f failure
		rows.Scan(&f.Date, &f.Attempts, &f.Error)
		failures = append(failures, f)
	}
	rows.Close()

	var entries int
	s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entries`).Scan(&entries)
	s.render(w, "status.html", map[string]any{
		"Title": "Status", "Scan": s.ix.Last(), "Kinds": kinds, "Entries": entries,
		"Grader": s.grader.Status(ctx), "Failures": failures, "KevURL": s.cfg.KevURL,
		"Exclude": strings.Join(s.cfg.Exclude, ", "), "Interval": s.cfg.ScanInterval.String(),
	})
}

func (s *Server) rescan(w http.ResponseWriter, r *http.Request) {
	if _, err := s.ix.Scan(r.Context()); err != nil {
		s.fail(w, err)
		return
	}
	s.grader.Wake()
	http.Redirect(w, r, "/status", http.StatusSeeOther)
}
