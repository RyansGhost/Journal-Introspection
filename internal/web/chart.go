package web

import (
	"fmt"
	"html"
	"html/template"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ryansghost/journal-introspection/internal/grader"
)

// The chart is server-rendered SVG. Colours come from CSS classes (s1..s8 for
// series, m0..m4 for mood bins) so the stylesheet can restyle them for dark
// mode and the page needs no inline styles.

const (
	chartW, chartH                           = 960.0, 250.0
	plotLeft, plotRight, plotTop, plotBottom = 40.0, 850.0, 12.0, 222.0
	day                                      = 24 * time.Hour
)

// window returns the first and last date shown, shifted when needed so the
// selected day is always inside it.
func window(days []*dayData, idx int, v viewState) (time.Time, time.Time) {
	first, last, sel := days[0].Date, days[len(days)-1].Date, days[idx].Date
	span := 365 * day
	switch v.Range {
	case "3m":
		span = 91 * day
	case "all":
		if last.Sub(first) < 30*day {
			return last.Add(-30 * day), last
		}
		return first, last
	}
	end := last
	if sel.Before(end.Add(-span)) {
		end = sel.Add(span / 2)
	}
	return end.Add(-span), end
}

type point struct {
	t time.Time
	v float64
}

// rolling returns, for each day that has a score, the mean of the scores in
// the n days ending on it.
func rolling(days []*dayData, dim string, n int) []point {
	var raw, out []point
	for _, d := range days {
		if v, ok := d.Scores[dim]; ok {
			raw = append(raw, point{d.Date, v})
		}
	}
	lo, sum := 0, 0.0
	for i, p := range raw {
		sum += p.v
		for raw[lo].t.Before(p.t.Add(-time.Duration(n-1) * day)) {
			sum -= raw[lo].v
			lo++
		}
		out = append(out, point{p.t, sum / float64(i-lo+1)})
	}
	return out
}

func (s *Server) chartSVG(days []*dayData, idx int, v viewState, chartDims []string) template.HTML {
	start, end := window(days, idx, v)
	span := end.Sub(start).Hours() / 24
	x := func(t time.Time) float64 {
		return plotLeft + (plotRight-plotLeft)*(t.Sub(start).Hours()/24)/span
	}
	y := func(val float64) float64 { return plotBottom - (plotBottom-plotTop)*val }
	inside := func(t time.Time) bool { return !t.Before(start) && !t.After(end) }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %g %g" role="img" aria-label="Journal scores over time">`, chartW, chartH)

	// Grid and axes: quiet, behind everything.
	for i := 0; i <= 4; i++ {
		gy := y(float64(i) / 4)
		fmt.Fprintf(&b, `<line class="grid" x1="%g" x2="%g" y1="%.1f" y2="%.1f"/>`, plotLeft, plotRight, gy, gy)
	}
	fmt.Fprintf(&b, `<text class="axis" x="%g" y="%.1f" text-anchor="end">high</text>`, plotLeft-6, y(1)+4)
	fmt.Fprintf(&b, `<text class="axis" x="%g" y="%.1f" text-anchor="end">low</text>`, plotLeft-6, y(0)+4)
	step := 1 // months between ticks
	if span > 3*365 {
		step = 12
	} else if span > 500 {
		step = 3
	}
	for t := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC); !t.After(end); t = t.AddDate(0, 1, 0) {
		if t.Before(start) || (int(t.Month())-1)%step != 0 {
			continue
		}
		label := t.Format("Jan")
		if t.Month() == time.January || step == 12 {
			label = t.Format("2006")
		}
		fmt.Fprintf(&b, `<line class="tick" x1="%.1f" x2="%.1f" y1="%g" y2="%g"/><text class="axis" x="%.1f" y="%g" text-anchor="middle">%s</text>`,
			x(t), x(t), plotBottom, plotBottom+4, x(t), plotBottom+18, label)
	}

	// The selected day.
	if sel := days[idx].Date; inside(sel) {
		fmt.Fprintf(&b, `<line class="sel" x1="%.1f" x2="%.1f" y1="%g" y2="%g"/>`, x(sel), x(sel), plotTop, plotBottom)
	}

	// One group per visible series.
	type endLabel struct {
		y    float64
		text string
		slot int
	}
	var labels []endLabel
	maxGap := time.Duration(max(45, 2*v.Avg)) * day
	for _, id := range v.Dims {
		slot := slices.Index(chartDims, id) + 1
		fmt.Fprintf(&b, `<g class="s%d">`, slot)
		if len(v.Dims) <= 2 && v.Avg > 1 { // raw daily values, faint, behind the average
			for _, d := range days {
				if val, ok := d.Scores[id]; ok && inside(d.Date) {
					fmt.Fprintf(&b, `<circle class="raw" cx="%.1f" cy="%.1f" r="2"/>`, x(d.Date), y(val))
				}
			}
		}
		var path strings.Builder
		var prev, lastPt *point
		for _, p := range rolling(days, id, v.Avg) {
			if !inside(p.t) {
				continue
			}
			cmd := "L"
			if prev == nil || p.t.Sub(prev.t) > maxGap {
				cmd = "M"
			}
			fmt.Fprintf(&path, "%s%.1f %.1f", cmd, x(p.t), y(p.v))
			prev, lastPt = &p, &p
		}
		if path.Len() > 0 {
			fmt.Fprintf(&b, `<path class="line" d="%s"/>`, path.String())
			fmt.Fprintf(&b, `<circle class="end" cx="%.1f" cy="%.1f" r="4"/>`, x(lastPt.t), y(lastPt.v))
			labels = append(labels, endLabel{y(lastPt.v), s.grader.Rubric.Dimension(id).Label, slot})
		}
		b.WriteString(`</g>`)
	}

	// Direct labels at the right edge, nudged apart, when there are few enough to read.
	if len(labels) <= 4 {
		sort.Slice(labels, func(i, j int) bool { return labels[i].y < labels[j].y })
		for i := range labels {
			if i > 0 && labels[i].y < labels[i-1].y+14 {
				labels[i].y = labels[i-1].y + 14
			}
			// The dot ties each label to its line; the text itself stays in ink.
			fmt.Fprintf(&b, `<g class="s%d"><circle cx="%g" cy="%.1f" r="3.5"/></g><text class="label" x="%g" y="%.1f">%s</text>`,
				labels[i].slot, plotRight+14, labels[i].y, plotRight+22, labels[i].y+4, html.EscapeString(labels[i].text))
		}
	}

	// A hover and click target for every day that has entries.
	colW := max((plotRight-plotLeft)/span, 5)
	for _, d := range days {
		if !inside(d.Date) {
			continue
		}
		var tip []string
		for _, id := range v.Dims {
			if val, ok := d.Scores[id]; ok {
				def := s.grader.Rubric.Dimension(id)
				tip = append(tip, def.Label+": "+strings.ToLower(def.LevelName(val*float64(len(def.Levels)-1))))
			}
		}
		if len(tip) == 0 {
			tip = []string{"not graded yet"}
		}
		title := d.Date.Format("Mon 2 Jan 2006")
		fmt.Fprintf(&b, `<a href="%s"><rect class="hit" x="%.1f" y="%g" width="%.1f" height="%g" data-x="%.1f" data-date="%s" data-tip="%s"><title>%s — %s</title></rect></a>`,
			html.EscapeString(v.url(d.Date.Format(dateFmt))), x(d.Date)-colW/2, plotTop, colW, plotBottom-plotTop,
			x(d.Date), title, html.EscapeString(strings.Join(tip, "\n")), title, html.EscapeString(strings.Join(tip, "; ")))
	}
	fmt.Fprintf(&b, `<line class="cross" x1="0" x2="0" y1="%g" y2="%g"/>`, plotTop, plotBottom)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

type tableView struct {
	Head []string
	Rows [][]string
	URLs []string
}

// chartTable is the same data as the chart, as text, newest first.
func chartTable(days []*dayData, idx int, v viewState, r *grader.Rubric) tableView {
	start, end := window(days, idx, v)
	t := tableView{Head: []string{"Date"}}
	for _, id := range v.Dims {
		t.Head = append(t.Head, r.Dimension(id).Label)
	}
	for i := len(days) - 1; i >= 0 && len(t.Rows) < 400; i-- {
		d := days[i]
		if d.Date.Before(start) || d.Date.After(end) {
			continue
		}
		row := []string{d.Date.Format(dateFmt)}
		for _, id := range v.Dims {
			cell := "–"
			if val, ok := d.Scores[id]; ok {
				def := r.Dimension(id)
				cell = def.LevelName(val * float64(len(def.Levels)-1))
			}
			row = append(row, cell)
		}
		t.Rows = append(t.Rows, row)
		t.URLs = append(t.URLs, v.url(d.Date.Format(dateFmt)))
	}
	return t
}

// yearStrip draws the selected day's calendar year as a week-by-weekday grid,
// each journal day coloured by mood.
func yearStrip(days []*dayData, sel time.Time, v viewState) template.HTML {
	const cell, gap, left, top = 13.0, 2.0, 30.0, 16.0
	byDate := map[string]*dayData{}
	for _, d := range days {
		byDate[d.Date.Format(dateFmt)] = d
	}
	jan1 := time.Date(sel.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	offset := (int(jan1.Weekday()) + 6) % 7 // weeks start on Monday

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="strip" viewBox="0 0 %g %g" role="img" aria-label="Mood by day, %d">`, left+54*(cell+gap), top+7*(cell+gap), sel.Year())
	for i, name := range []string{"Mon", "", "Wed", "", "Fri", "", "Sun"} {
		if name != "" {
			fmt.Fprintf(&b, `<text class="axis" x="0" y="%.1f">%s</text>`, top+float64(i)*(cell+gap)+cell-2, name)
		}
	}
	for t := jan1; t.Year() == sel.Year(); t = t.AddDate(0, 0, 1) {
		n := t.YearDay() - 1 + offset
		cx, cy := left+float64(n/7)*(cell+gap), top+float64(n%7)*(cell+gap)
		if t.Day() == 1 {
			fmt.Fprintf(&b, `<text class="axis" x="%.1f" y="10">%s</text>`, cx, t.Format("Jan"))
		}
		key := t.Format(dateFmt)
		d := byDate[key]
		class, tip := "cell e", ""
		if d != nil {
			class, tip = "cell u", "not graded yet"
			if mood, ok := d.Scores["mood"]; ok {
				bin := min(int(mood*5), 4)
				class = fmt.Sprintf("cell m%d", bin)
				tip = "mood " + []string{"very low", "low", "neutral", "good", "very good"}[bin]
			}
		}
		if t.Equal(sel) {
			class += " cur"
		}
		rect := fmt.Sprintf(`<rect class="%s" x="%.1f" y="%.1f" width="%g" height="%g" rx="2"`, class, cx, cy, cell, cell)
		if d == nil {
			b.WriteString(rect + `/>`)
			continue
		}
		fmt.Fprintf(&b, `<a href="%s">%s><title>%s — %s</title></rect></a>`, html.EscapeString(v.url(key)), rect, t.Format("Mon 2 Jan 2006"), tip)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
