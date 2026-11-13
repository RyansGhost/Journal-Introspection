package web

import (
	"context"
	"database/sql"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"journal-introspection/internal/orgrender"
)

const dateFmt = "2006-01-02"

// dayData is everything known about one journal day.
type dayData struct {
	Date   time.Time
	Words  int
	Scores map[string]float64 // dimension id -> 0..1, word-weighted across the day's entries
	Conf   map[string]float64
}

// loadDays returns every journal day in date order, with the day's scores
// under the current rubric and model where they exist.
func (s *Server) loadDays(ctx context.Context) ([]*dayData, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT date, SUM(word_count) FROM entries GROUP BY date ORDER BY date`)
	if err != nil {
		return nil, err
	}
	var days []*dayData
	byDate := map[string]*dayData{}
	for rows.Next() {
		var d string
		day := &dayData{Scores: map[string]float64{}, Conf: map[string]float64{}}
		if err := rows.Scan(&d, &day.Words); err != nil {
			rows.Close()
			return nil, err
		}
		if day.Date, err = time.Parse(dateFmt, d); err != nil {
			continue
		}
		days = append(days, day)
		byDate[d] = day
	}
	rows.Close()

	rows, err = s.db.QueryContext(ctx, `
		SELECT e.date, g.dimension,
		       SUM(g.score * MAX(e.word_count, 1)) / SUM(MAX(e.word_count, 1)),
		       SUM(g.confidence * MAX(e.word_count, 1)) / SUM(MAX(e.word_count, 1))
		FROM entries e
		JOIN grades g ON g.entry_hash = e.content_hash AND g.rubric_version = ? AND g.model = ?
		WHERE g.score IS NOT NULL
		GROUP BY e.date, g.dimension`, s.grader.Rubric.Version, s.grader.Model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d, dim string
		var score, conf float64
		if err := rows.Scan(&d, &dim, &score, &conf); err != nil {
			return nil, err
		}
		day, def := byDate[d], s.grader.Rubric.Dimension(dim)
		if day == nil || def == nil {
			continue
		}
		day.Scores[dim] = def.Normalise(score)
		day.Conf[dim] = conf
	}
	return days, rows.Err()
}

type entryView struct {
	Time, Heading string
	Tags          []string
	Body          template.HTML
	Words         int
	Summary       string // one line of grades, empty until graded
}

type meterView struct {
	Label, Level string
	Width        float64 // 0..100
	Faint        bool    // low confidence
}

// viewState is the chart configuration carried in the query string.
type viewState struct {
	Date  string
	Range string // 3m | 1y | all
	Avg   int    // rolling-average window in days
	Dims  []string
}

func (v viewState) url(date string) string {
	q := url.Values{}
	if v.Range != "1y" {
		q.Set("range", v.Range)
	}
	if v.Avg != 7 {
		q.Set("avg", strconv.Itoa(v.Avg))
	}
	if !slices.Equal(v.Dims, []string{"mood"}) {
		q.Set("dims", strings.Join(v.Dims, ","))
	}
	u := "/journal/" + date
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

type linkView struct {
	Label, URL string
	Active     bool
	Slot       int
}

func (s *Server) journal(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	days, err := s.loadDays(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(days) == 0 {
		s.render(w, "journal.html", map[string]any{"Title": "Journal", "Empty": true})
		return
	}

	// Which day: the one in the URL, or the most recent.
	idx := len(days) - 1
	if want := r.PathValue("date"); want != "" {
		t, err := time.Parse(dateFmt, want)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		idx = slices.IndexFunc(days, func(d *dayData) bool { return d.Date.Equal(t) })
		if idx < 0 {
			http.NotFound(w, r)
			return
		}
	}
	day := days[idx]

	// Chart settings from the query string, with safe defaults.
	q := r.URL.Query()
	v := viewState{Date: day.Date.Format(dateFmt), Range: "1y", Avg: 7}
	if rg := q.Get("range"); rg == "3m" || rg == "all" {
		v.Range = rg
	}
	switch q.Get("avg") {
	case "1":
		v.Avg = 1
	case "30":
		v.Avg = 30
	}
	var chartDims []string
	for _, d := range s.grader.Rubric.Dimensions {
		if d.Chart && d.Type == "score" {
			chartDims = append(chartDims, d.ID)
		}
	}
	for _, id := range strings.Split(q.Get("dims"), ",") {
		if slices.Contains(chartDims, id) && !slices.Contains(v.Dims, id) {
			v.Dims = append(v.Dims, id)
		}
	}
	if len(v.Dims) == 0 {
		v.Dims = []string{"mood"}
		if !slices.Contains(chartDims, "mood") && len(chartDims) > 0 {
			v.Dims = chartDims[:1]
		}
	}

	// Controls.
	var ranges, avgs, legend []linkView
	for _, o := range []struct{ id, label string }{{"3m", "3 months"}, {"1y", "1 year"}, {"all", "All"}} {
		nv := v
		nv.Range = o.id
		ranges = append(ranges, linkView{Label: o.label, URL: nv.url(v.Date), Active: v.Range == o.id})
	}
	for _, o := range []struct {
		n     int
		label string
	}{{1, "Daily"}, {7, "7-day average"}, {30, "30-day average"}} {
		nv := v
		nv.Avg = o.n
		avgs = append(avgs, linkView{Label: o.label, URL: nv.url(v.Date), Active: v.Avg == o.n})
	}
	for i, id := range chartDims {
		nv := v
		on := slices.Contains(v.Dims, id)
		if on && len(v.Dims) > 1 {
			nv.Dims = slices.DeleteFunc(slices.Clone(v.Dims), func(x string) bool { return x == id })
		} else if !on {
			// Keep rubric order so a series never changes colour.
			nv.Dims = nil
			for _, c := range chartDims {
				if c == id || slices.Contains(v.Dims, c) {
					nv.Dims = append(nv.Dims, c)
				}
			}
		}
		legend = append(legend, linkView{Label: s.grader.Rubric.Dimension(id).Label, URL: nv.url(v.Date), Active: on, Slot: i + 1})
	}

	entries, meters, flags, theme, err := s.loadDay(ctx, v.Date)
	if err != nil {
		s.fail(w, err)
		return
	}

	data := map[string]any{
		"Title":   day.Date.Format("Monday, 2 January 2006"),
		"Date":    v.Date,
		"Entries": entries,
		"Meters":  meters,
		"Flags":   flags,
		"Theme":   theme,
		"Graded":  len(day.Scores) > 0,
		"Grading": s.grader.Status(ctx).Enabled,
		"Ranges":  ranges,
		"Avgs":    avgs,
		"Legend":  legend,
		"Chart":   s.chartSVG(days, idx, v, chartDims),
		"Table":   chartTable(days, idx, v, s.grader.Rubric),
		"Year":    day.Date.Year(),
		"Strip":   yearStrip(days, day.Date, v),
		"File":    "journal/" + day.Date.Format("20060102") + ".org",
	}
	if idx > 0 {
		data["Prev"] = v.url(days[idx-1].Date.Format(dateFmt))
	}
	if idx < len(days)-1 {
		data["Next"] = v.url(days[idx+1].Date.Format(dateFmt))
	}
	s.render(w, "journal.html", data)
}

// loadDay returns the rendered entries for one date, plus the day's meters
// (score dimensions), yes/no flags that are more likely than not, and theme.
func (s *Server) loadDay(ctx context.Context, date string) (entries []entryView, meters []meterView, flags []string, theme string, err error) {
	rubric := s.grader.Rubric
	type row struct {
		entryView
		hash, body string
	}
	rows, err := s.db.QueryContext(ctx, `SELECT time, heading, tags, body_org, word_count, content_hash FROM entries WHERE date=? ORDER BY file_id, seq`, date)
	if err != nil {
		return
	}
	var list []row
	for rows.Next() {
		var e row
		var tags string
		if err = rows.Scan(&e.Time, &e.Heading, &tags, &e.body, &e.Words, &e.hash); err != nil {
			rows.Close()
			return
		}
		e.Tags = strings.Fields(tags)
		list = append(list, e)
	}
	rows.Close()

	type agg struct{ sum, conf, weight float64 }
	totals := map[string]*agg{}
	themes := map[string]float64{}
	for _, e := range list {
		var grows *sql.Rows
		grows, err = s.db.QueryContext(ctx, `SELECT dimension, score, label, confidence FROM grades WHERE entry_hash=? AND rubric_version=? AND model=?`,
			e.hash, rubric.Version, s.grader.Model)
		if err != nil {
			return
		}
		var parts []string
		w := float64(max(e.Words, 1))
		got := map[string]string{}
		for grows.Next() {
			var dim, label string
			var score sql.NullFloat64
			var conf float64
			if err = grows.Scan(&dim, &score, &label, &conf); err != nil {
				grows.Close()
				return
			}
			def := rubric.Dimension(dim)
			if def == nil {
				continue
			}
			switch def.Type {
			case "choice":
				themes[label] += w
				got[dim] = def.Label + ": " + label
			case "score":
				got[dim] = def.Label + ": " + strings.ToLower(def.LevelName(score.Float64))
				fallthrough
			default:
				a := totals[dim]
				if a == nil {
					a = &agg{}
					totals[dim] = a
				}
				a.sum += score.Float64 * w
				a.conf += conf * w
				a.weight += w
			}
		}
		grows.Close()
		// The entry's one-line summary: mood and theme are enough at a glance.
		for _, id := range []string{"mood", "theme"} {
			if got[id] != "" {
				parts = append(parts, got[id])
			}
		}
		e.Summary = strings.Join(parts, " · ")
		e.Body = orgrender.HTML(e.body, false, s.resolver(ctx, "journal"))
		entries = append(entries, e.entryView)
	}

	for _, d := range rubric.Dimensions {
		a := totals[d.ID]
		if a == nil || a.weight == 0 {
			continue
		}
		mean := a.sum / a.weight
		switch d.Type {
		case "score":
			meters = append(meters, meterView{Label: d.Label, Level: d.LevelName(mean), Width: 100 * d.Normalise(mean), Faint: a.conf/a.weight < 0.25})
		case "noul":
			if mean >= 0.5 {
				flags = append(flags, d.Label)
			}
		}
	}
	best := 0.0
	for k, v := range themes {
		if v > best || (v == best && k < theme) {
			best, theme = v, k
		}
	}
	return
}
