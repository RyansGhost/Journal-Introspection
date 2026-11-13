package grader

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ryansghost/journal-introspection/internal/store"
)

type Status struct {
	Enabled     bool
	Model       string
	Rubric      string
	Graded      int // distinct entries graded with the current rubric and model
	Pending     int
	Failed      int // entries Kev rejected; retried with a growing delay
	LastSuccess time.Time
	LastError   string
	LastErrorAt time.Time
}

type Grader struct {
	db     *store.DB
	Rubric *Rubric
	Model  string     // the label grades are stored under
	kev    *kevClient // nil when KEV_URL is not set

	wake chan struct{}
	mu   sync.Mutex
	st   Status
}

// New makes a grader. model is the name sent to Kev; label is what grades are
// stored under, so changing it regrades everything.
func New(db *store.DB, rubric *Rubric, kevURL, model, label string) *Grader {
	g := &Grader{db: db, Rubric: rubric, Model: label, wake: make(chan struct{}, 1)}
	if kevURL != "" {
		g.kev = newKevClient(kevURL, model)
	}
	return g
}

// Wake tells the worker there may be new entries to grade.
func (g *Grader) Wake() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

func (g *Grader) Status(ctx context.Context) Status {
	g.mu.Lock()
	st := g.st
	g.mu.Unlock()
	st.Enabled, st.Model, st.Rubric = g.kev != nil, g.Model, g.Rubric.Version
	g.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(g.entry_hash IS NOT NULL), 0), COALESCE(SUM(f.entry_hash IS NOT NULL AND g.entry_hash IS NULL), 0)
		FROM (SELECT DISTINCT content_hash AS h FROM entries) e
		LEFT JOIN graded g ON g.entry_hash = e.h AND g.rubric_version = ?1 AND g.model = ?2
		LEFT JOIN grade_failures f ON f.entry_hash = e.h AND f.rubric_version = ?1 AND f.model = ?2`,
		g.Rubric.Version, g.Model).Scan(&st.Pending, &st.Graded, &st.Failed)
	st.Pending -= st.Graded
	return st
}

// Run grades entries one at a time, newest first, until ctx is cancelled.
// One request at a time keeps the Mac mini responsive.
func (g *Grader) Run(ctx context.Context) {
	if g.kev == nil {
		slog.Info("grading disabled: KEV_URL is not set")
		return
	}
	backoff := 5 * time.Second
	for ctx.Err() == nil {
		wait := 5 * time.Minute // idle poll, in case a retry has come due
		worked, err := g.gradeNext(ctx)
		switch {
		case err != nil && !errors.Is(err, errRejected):
			// Kev is down or unreachable: wait and try the same entry again.
			g.note(err)
			wait = backoff
			backoff = min(backoff*2, 10*time.Minute)
		case worked:
			backoff = 5 * time.Second
			continue
		}
		select {
		case <-ctx.Done():
		case <-g.wake:
		case <-time.After(wait):
		}
	}
}

func (g *Grader) note(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err == nil {
		g.st.LastSuccess = time.Now()
		return
	}
	if g.st.LastError != err.Error() {
		slog.Warn("grading", "err", err)
	}
	g.st.LastError, g.st.LastErrorAt = err.Error(), time.Now()
}

// gradeNext grades the newest ungraded entry. It reports whether it did any
// work; a rejected entry counts as work so the loop moves on to the next one.
func (g *Grader) gradeNext(ctx context.Context) (bool, error) {
	var hash, date, clock, body string
	err := g.db.QueryRowContext(ctx, `
		SELECT e.content_hash, e.date, e.time, e.body_org FROM entries e
		WHERE NOT EXISTS (SELECT 1 FROM graded g WHERE g.entry_hash = e.content_hash AND g.rubric_version = ?1 AND g.model = ?2)
		  AND NOT EXISTS (SELECT 1 FROM grade_failures f WHERE f.entry_hash = e.content_hash AND f.rubric_version = ?1 AND f.model = ?2 AND f.next_try_at > ?3)
		ORDER BY e.date DESC, e.seq DESC LIMIT 1`,
		g.Rubric.Version, g.Model, time.Now().Unix()).Scan(&hash, &date, &clock, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	answers, err := g.grade(ctx, date, clock, body)
	if errors.Is(err, errRejected) {
		g.note(err)
		_, dbErr := g.db.ExecContext(ctx, `
			INSERT INTO grade_failures(entry_hash, rubric_version, model, attempts, last_error, next_try_at) VALUES(?,?,?,1,?,?)
			ON CONFLICT DO UPDATE SET attempts = attempts + 1, last_error = excluded.last_error,
				next_try_at = ? + 3600 * (attempts + 1)`,
			hash, g.Rubric.Version, g.Model, err.Error(), time.Now().Unix()+3600, time.Now().Unix())
		return true, dbErr
	}
	if err != nil {
		return false, err
	}
	if err := g.save(ctx, hash, answers); err != nil {
		return false, err
	}
	g.note(nil)
	return true, nil
}

func (g *Grader) grade(ctx context.Context, date, clock, body string) (map[string]Answer, error) {
	intro := "Personal journal entry written on " + date
	if clock != "" {
		intro += " at " + clock
	}
	var parts []map[string]Answer
	var weights []float64
	for _, c := range chunk(body, maxChunkWords) {
		a, err := g.kev.ask(ctx, g.Rubric, intro+".\n\n"+c)
		if err != nil {
			return nil, err
		}
		parts = append(parts, a)
		weights = append(weights, float64(max(len(strings.Fields(c)), 1)))
	}
	return combine(g.Rubric, parts, weights), nil
}

func (g *Grader) save(ctx context.Context, hash string, answers map[string]Answer) error {
	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for id, a := range answers {
		probs, _ := json.Marshal(a.Probs)
		if _, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO grades(entry_hash, rubric_version, model, dimension, score, label, confidence, probs_json)
			VALUES(?,?,?,?,?,?,?,?)`, hash, g.Rubric.Version, g.Model, id, a.Score, a.Label, a.Confidence, string(probs)); err != nil {
			return fmt.Errorf("save grade: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO graded(entry_hash, rubric_version, model, graded_at) VALUES(?,?,?,?)`,
		hash, g.Rubric.Version, g.Model, time.Now().Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM grade_failures WHERE entry_hash=? AND rubric_version=? AND model=?`,
		hash, g.Rubric.Version, g.Model); err != nil {
		return err
	}
	return tx.Commit()
}
