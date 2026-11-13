// Package grader sends journal entries to a Kev server and stores the scores.
package grader

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

//go:embed rubric.json
var builtinRubric []byte

// Dimension is one question asked about every entry. The JSON shape of
// type, instructions and criteria is exactly what Kev expects.
type Dimension struct {
	ID           string          `json:"id"`
	Label        string          `json:"label"`
	Type         string          `json:"type"` // score | noul | choice
	Chart        bool            `json:"chart"`
	Instructions string          `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`

	Levels []string `json:"-"` // score: the ordered level descriptions
}

type Rubric struct {
	Version    string // short hash of the rubric file; changing the file regrades everything
	Dimensions []Dimension
}

// LoadRubric reads the rubric at path, or the built-in one when path is empty.
func LoadRubric(path string) (*Rubric, error) {
	data := builtinRubric
	if path != "" {
		var err error
		if data, err = os.ReadFile(path); err != nil {
			return nil, err
		}
	}
	var raw struct {
		Dimensions []Dimension `json:"dimensions"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("rubric: %w", err)
	}
	seen := map[string]bool{}
	for i := range raw.Dimensions {
		d := &raw.Dimensions[i]
		if d.ID == "" || seen[d.ID] {
			return nil, fmt.Errorf("rubric: missing or duplicate id %q", d.ID)
		}
		seen[d.ID] = true
		switch d.Type {
		case "score":
			if err := json.Unmarshal(d.Criteria, &d.Levels); err != nil || len(d.Levels) < 2 {
				return nil, fmt.Errorf("rubric: %s needs a list of at least two levels", d.ID)
			}
		case "choice":
			var opts map[string]any
			if err := json.Unmarshal(d.Criteria, &opts); err != nil || len(opts) == 0 {
				return nil, fmt.Errorf("rubric: %s needs an object of options", d.ID)
			}
		case "noul":
		default:
			return nil, fmt.Errorf("rubric: %s has unknown type %q", d.ID, d.Type)
		}
	}
	if len(raw.Dimensions) == 0 {
		return nil, fmt.Errorf("rubric: no dimensions")
	}
	sum := sha256.Sum256(data)
	return &Rubric{Version: hex.EncodeToString(sum[:])[:12], Dimensions: raw.Dimensions}, nil
}

// Dimension returns the dimension with the given id, or nil.
func (r *Rubric) Dimension(id string) *Dimension {
	for i := range r.Dimensions {
		if r.Dimensions[i].ID == id {
			return &r.Dimensions[i]
		}
	}
	return nil
}

// Normalise maps a stored score onto 0..1: a score dimension's mean level
// index divided by its top level; a noul probability unchanged.
func (d *Dimension) Normalise(score float64) float64 {
	if d.Type == "score" && len(d.Levels) > 1 {
		score /= float64(len(d.Levels) - 1)
	}
	return min(max(score, 0), 1)
}

// LevelName returns the description of the level nearest to a stored score.
func (d *Dimension) LevelName(score float64) string {
	if d.Type != "score" {
		return ""
	}
	i := int(score + 0.5)
	return d.Levels[min(max(i, 0), len(d.Levels)-1)]
}
