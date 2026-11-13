// fakekev is a stand-in for the Kev server, for development and tests on a
// machine without the real model. It answers POST /v1/systemone in Kev's
// response shape, scoring by crude keyword counts. Its output means nothing.
package main

import (
	"encoding/json"
	"hash/fnv"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
)

var (
	good = strings.Fields("great love loved enjoy enjoying grateful thankful happy fun good wonderful proud blessed peace calm rested energised")
	bad  = strings.Fields("tired exhausted anxious worried worry stressed sad angry awful terrible lonely afraid overwhelmed argument sick deadline")
)

func count(text string, words []string) (n int) {
	for _, w := range words {
		n += strings.Count(text, w)
	}
	return
}

func main() {
	http.HandleFunc("POST /v1/systemone", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			State     json.RawMessage `json:"state"`
			Model     string          `json:"model"`
			Questions map[string]struct {
				Type     string          `json:"type"`
				Criteria json.RawMessage `json:"criteria"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		text := strings.ToLower(string(req.State))
		// tone runs from 0 (bleak) to 1 (bright); jitter keeps days from looking identical.
		tone := 0.5 + 0.12*float64(count(text, good)-count(text, bad))
		answers := map[string]any{}
		for id, q := range req.Questions {
			h := fnv.New32a()
			h.Write([]byte(id + text))
			jitter := float64(h.Sum32()%1000)/1000 - 0.5
			v := min(max(tone+0.25*jitter, 0), 1)
			if id == "anxiety" || id == "stress" {
				v = 1 - v
			}
			switch q.Type {
			case "score":
				var levels []string
				json.Unmarshal(q.Criteria, &levels)
				score := v * float64(len(levels)-1)
				legend, probs := map[string]string{}, map[string]float64{}
				for i, l := range levels {
					legend[strconv.Itoa(i)] = l
					probs[strconv.Itoa(i)] = max(0, 1-max(score-float64(i), float64(i)-score))
				}
				answers[id] = map[string]any{"type": "score", "score": score, "confidence": 0.5, "legend": legend, "probabilities": probs}
			case "choice":
				var opts map[string]any
				json.Unmarshal(q.Criteria, &opts)
				names := make([]string, 0, len(opts))
				for k := range opts {
					names = append(names, k)
				}
				sort.Strings(names)
				pick := names[int(h.Sum32())%len(names)]
				for _, n := range names {
					if strings.Contains(text, n) {
						pick = n
					}
				}
				answers[id] = map[string]any{"type": "choice", "choice": pick, "confidence": 0.4, "probabilities": map[string]float64{pick: 1}}
			default:
				p := 0.5 + jitter*0.8
				if key, ok := strings.CutPrefix(id, "mentions_"); ok {
					p = 0.05
					if strings.Contains(text, key) || (key == "sleep" && strings.Contains(text, "slept")) {
						p = 0.95
					}
				}
				answers[id] = map[string]any{"type": "noul", "noul": p}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"model": req.Model, "answers": answers})
	})
	addr := ":8009"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}
	log.Println("fake Kev listening on", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
