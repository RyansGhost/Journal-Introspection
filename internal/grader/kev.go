package grader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Answer is one dimension's result for one entry.
type Answer struct {
	Score      *float64 // nil for choice
	Label      string   // choice only
	Confidence float64
	Probs      map[string]float64
}

// errRejected marks a response that will not improve on retry soon (HTTP 4xx
// or a malformed answer), as opposed to the server being down.
var errRejected = errors.New("kev rejected the request")

type kevClient struct {
	url, model string
	http       *http.Client
}

func newKevClient(url, model string) *kevClient {
	return &kevClient{url: url, model: model, http: &http.Client{Timeout: 5 * time.Minute}}
}

type kevQuestion struct {
	Type         string          `json:"type"`
	Instructions string          `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria,omitempty"`
}

type kevAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Noul          *float64           `json:"noul"`
	Score         *float64           `json:"score"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// ask sends one piece of text with every rubric question to POST /v1/systemone.
func (c *kevClient) ask(ctx context.Context, r *Rubric, text string) (map[string]Answer, error) {
	questions := map[string]kevQuestion{}
	for _, d := range r.Dimensions {
		questions[d.ID] = kevQuestion{Type: d.Type, Instructions: d.Instructions, Criteria: d.Criteria}
	}
	body, _ := json.Marshal(map[string]any{"state": text, "model": c.model, "questions": questions})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("kev: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data[:min(len(data), 300)])))
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, fmt.Errorf("%w: %v", errRejected, err)
		}
		return nil, err
	}
	var parsed struct {
		Answers map[string]kevAnswer `json:"answers"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("%w: bad JSON: %v", errRejected, err)
	}
	out := map[string]Answer{}
	for _, d := range r.Dimensions {
		a, ok := parsed.Answers[d.ID]
		if !ok {
			return nil, fmt.Errorf("%w: no answer for %q", errRejected, d.ID)
		}
		ans := Answer{Confidence: a.Confidence, Probs: a.Probabilities}
		switch d.Type {
		case "score":
			if a.Score == nil {
				return nil, fmt.Errorf("%w: %q has no score", errRejected, d.ID)
			}
			ans.Score = a.Score
		case "noul":
			if a.Noul == nil {
				return nil, fmt.Errorf("%w: %q has no noul", errRejected, d.ID)
			}
			ans.Score = a.Noul
			// Kev gives no confidence for yes/no; use distance from a coin flip.
			ans.Confidence = 2 * max(*a.Noul-0.5, 0.5-*a.Noul)
		case "choice":
			ans.Label = a.Choice
		}
		out[d.ID] = ans
	}
	return out, nil
}

// maxChunkWords keeps each request inside Kev's 8,192-token validated context
// (the smaller models), leaving room for the questions.
const maxChunkWords = 3000

// chunk splits long text on blank lines into pieces of at most maxWords words.
func chunk(text string, maxWords int) []string {
	if len(strings.Fields(text)) <= maxWords {
		return []string{text}
	}
	var out, cur []string
	n := 0
	for _, para := range strings.Split(text, "\n\n") {
		w := len(strings.Fields(para))
		if n > 0 && n+w > maxWords {
			out = append(out, strings.Join(cur, "\n\n"))
			cur, n = nil, 0
		}
		cur = append(cur, para)
		n += w
	}
	if n > 0 {
		out = append(out, strings.Join(cur, "\n\n"))
	}
	return out
}

// combine merges per-chunk answers into one, weighting each chunk by its word count.
func combine(r *Rubric, parts []map[string]Answer, weights []float64) map[string]Answer {
	if len(parts) == 1 {
		return parts[0]
	}
	var total float64
	for _, w := range weights {
		total += w
	}
	out := map[string]Answer{}
	for _, d := range r.Dimensions {
		merged := Answer{Probs: map[string]float64{}}
		var score float64
		for i, p := range parts {
			a, w := p[d.ID], weights[i]/total
			merged.Confidence += a.Confidence * w
			if a.Score != nil {
				score += *a.Score * w
			}
			for k, v := range a.Probs {
				merged.Probs[k] += v * w
			}
		}
		if d.Type == "choice" {
			best := -1.0
			for k, v := range merged.Probs {
				if v > best || (v == best && k < merged.Label) {
					best, merged.Label = v, k
				}
			}
			if merged.Label == "" {
				merged.Label = parts[0][d.ID].Label
			}
		} else {
			merged.Score = &score
		}
		out[d.ID] = merged
	}
	return out
}
