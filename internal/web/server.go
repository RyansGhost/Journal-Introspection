// Package web serves the reader, the timeline and the login.
package web

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"

	"journal-introspection/internal/config"
	"journal-introspection/internal/grader"
	"journal-introspection/internal/indexer"
	"journal-introspection/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Server struct {
	cfg    config.Config
	db     *store.DB
	ix     *indexer.Indexer
	grader *grader.Grader
	root   *os.Root // every file read goes through this, so nothing outside ORG_ROOT is reachable
	pages  map[string]*template.Template
}

func New(cfg config.Config, db *store.DB, ix *indexer.Indexer, g *grader.Grader) (*Server, error) {
	root, err := os.OpenRoot(cfg.OrgRoot)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, db: db, ix: ix, grader: g, root: root, pages: map[string]*template.Template{}}
	names, _ := fs.Glob(templateFS, "templates/*.html")
	for _, n := range names {
		if n == "templates/base.html" {
			continue
		}
		t, err := template.ParseFS(templateFS, "templates/base.html", n)
		if err != nil {
			return nil, err
		}
		s.pages[n[len("templates/"):]] = t
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /{$}", s.journal)
	mux.HandleFunc("GET /journal/{date}", s.journal)
	mux.HandleFunc("GET /files/{path...}", s.files)
	mux.HandleFunc("GET /search", s.search)
	mux.HandleFunc("GET /status", s.status)
	mux.HandleFunc("POST /rescan", s.rescan)
	return s.protect(mux)
}

// render executes a page into a buffer first, so a template error never
// leaves half a page on the wire.
func (s *Server) render(w http.ResponseWriter, page string, data map[string]any) {
	if _, ok := data["AuthDisabled"]; !ok {
		data["AuthDisabled"] = s.cfg.AuthDisabled
	}
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, "base", data); err != nil {
		slog.Error("render", "page", page, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	slog.Error("request failed", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}
