package grader

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ryansghost/journal-introspection/internal/store"
)

// kevStub answers in the shape shown in the Kev README.
func kevStub(t *testing.T, calls *atomic.Int32, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if status != http.StatusOK {
			http.Error(w, "nope", status)
			return
		}
		var req struct {
			State     string
			Model     string
			Questions map[string]struct{ Type string }
		}
		json.NewDecoder(r.Body).Decode(&req)
		if !strings.Contains(req.State, "Personal journal entry written on 2026-11-13") {
			t.Errorf("state lacks the date line: %q", req.State)
		}
		answers := map[string]any{}
		for id, q := range req.Questions {
			switch q.Type {
			case "score":
				answers[id] = map[string]any{"type": "score", "score": 3.0, "confidence": 0.6, "probabilities": map[string]float64{"3": 1}}
			case "noul":
				answers[id] = map[string]any{"type": "noul", "noul": 0.9}
			case "choice":
				answers[id] = map[string]any{"type": "choice", "choice": "work", "confidence": 0.5, "probabilities": map[string]float64{"work": 1}}
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"model": req.Model, "answers": answers})
	}))
}

func setup(t *testing.T) *store.DB {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.Exec(`INSERT INTO files(id, path, kind, title, mtime, size, sha256, indexed_at) VALUES(1,'journal/20261113.org','journal','x',0,0,'',0)`)
	db.Exec(`INSERT INTO entries(file_id, date, time, heading, body_org, tags, props_json, word_count, content_hash, seq)
		VALUES(1,'2026-11-13','07:37','','Last night was great.','','{}',4,'hash-a',0)`)
	return db
}

func TestGradeAndRegrade(t *testing.T) {
	db := setup(t)
	rubric, err := LoadRubric("")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	srv := kevStub(t, &calls, http.StatusOK)
	defer srv.Close()
	ctx := context.Background()
	g := New(db, rubric, srv.URL, "kev-test")

	if worked, err := g.gradeNext(ctx); !worked || err != nil {
		t.Fatalf("first grade: worked=%v err=%v", worked, err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM grades WHERE entry_hash='hash-a'`).Scan(&n)
	if n != len(rubric.Dimensions) {
		t.Errorf("want %d grades, got %d", len(rubric.Dimensions), n)
	}
	var conf float64
	db.QueryRow(`SELECT confidence FROM grades WHERE dimension='rumination'`).Scan(&conf)
	if conf < 0.79 || conf > 0.81 {
		t.Errorf("noul confidence: want 0.8, got %v", conf)
	}
	// Already graded: no second request.
	if worked, _ := g.gradeNext(ctx); worked || calls.Load() != 1 {
		t.Errorf("graded entry was sent again (calls=%d)", calls.Load())
	}
	if st := g.Status(ctx); st.Graded != 1 || st.Pending != 0 {
		t.Errorf("status: %+v", st)
	}
	// A different model (or rubric version) grades again and keeps the old rows.
	g2 := New(db, rubric, srv.URL, "kev-other")
	if worked, _ := g2.gradeNext(ctx); !worked {
		t.Error("new model did not regrade")
	}
	db.QueryRow(`SELECT COUNT(DISTINCT model) FROM grades`).Scan(&n)
	if n != 2 {
		t.Errorf("want grades from 2 models, got %d", n)
	}
}

func TestRejectedEntryIsParked(t *testing.T) {
	db := setup(t)
	rubric, _ := LoadRubric("")
	var calls atomic.Int32
	srv := kevStub(t, &calls, http.StatusUnprocessableEntity)
	defer srv.Close()
	g := New(db, rubric, srv.URL, "kev-test")
	ctx := context.Background()
	if worked, err := g.gradeNext(ctx); !worked || err != nil {
		t.Fatalf("rejection should be recorded, not returned: worked=%v err=%v", worked, err)
	}
	if worked, _ := g.gradeNext(ctx); worked || calls.Load() != 1 {
		t.Error("rejected entry retried immediately")
	}
	if st := g.Status(ctx); st.Failed != 1 || st.Pending != 1 {
		t.Errorf("status: %+v", st)
	}
}

func TestServerDownIsNotHeldAgainstTheEntry(t *testing.T) {
	db := setup(t)
	rubric, _ := LoadRubric("")
	var calls atomic.Int32
	srv := kevStub(t, &calls, http.StatusBadGateway)
	defer srv.Close()
	g := New(db, rubric, srv.URL, "kev-test")
	if worked, err := g.gradeNext(context.Background()); worked || err == nil {
		t.Fatalf("want an error and no progress, got worked=%v err=%v", worked, err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM grade_failures`).Scan(&n)
	if n != 0 {
		t.Error("an outage was recorded as the entry's failure")
	}
}

func TestChunkAndCombine(t *testing.T) {
	text := strings.Repeat("word ", 10) + "\n\n" + strings.Repeat("word ", 10) + "\n\n" + strings.Repeat("word ", 5)
	parts := chunk(text, 12)
	if len(parts) != 3 {
		t.Fatalf("want 3 chunks, got %d", len(parts))
	}
	rubric, _ := LoadRubric("")
	lo, hi := 0.0, 4.0
	mk := func(s *float64, theme string) map[string]Answer {
		m := map[string]Answer{}
		for _, d := range rubric.Dimensions {
			m[d.ID] = Answer{Score: s, Confidence: 0.5}
		}
		m["theme"] = Answer{Label: theme, Probs: map[string]float64{theme: 1}}
		return m
	}
	got := combine(rubric, []map[string]Answer{mk(&lo, "work"), mk(&hi, "faith")}, []float64{1, 3})
	if s := *got["mood"].Score; s != 3 {
		t.Errorf("weighted mean: want 3, got %v", s)
	}
	if got["theme"].Label != "faith" {
		t.Errorf("theme: want the heavier chunk's, got %q", got["theme"].Label)
	}
}
