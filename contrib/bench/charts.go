package main

import (
	"fmt"
	"html"
	"math"
	"slices"
	"strings"
)

// SVG chart primitives. Charts are generated as inline SVG that reference CSS
// custom properties (defined in the report's <style>) so a single light/dark
// theme swap recolors everything. Every mark carries a <title> for a native
// hover tooltip; every chart is paired with a data table in the report for the
// color-blind / print / raw-figures case.

const (
	chartW = 760
	chartH = 380
	marL   = 64
	marR   = 24
	marT   = 28
	marB   = 74
)

func plotW() float64 { return chartW - marL - marR }
func plotH() float64 { return chartH - marT - marB }

// catColors is the fixed categorical order from the validated reference palette
// (blue, green, magenta, yellow, aqua, orange), mapped to CSS vars.
var catColors = []string{
	"var(--cat-1)", "var(--cat-2)", "var(--cat-3)",
	"var(--cat-4)", "var(--cat-5)", "var(--cat-6)",
}

type barGroup struct {
	name   string
	color  string
	values []float64 // one per category
}

// groupedBars renders categories on x with one bar per group in each category.
func groupedBars(title, yLabel string, cats []string, groups []barGroup) string {
	var maxVal float64
	for _, g := range groups {
		if len(g.values) > 0 {
			maxVal = max(maxVal, slices.Max(g.values))
		}
	}
	maxVal = niceMax(maxVal)

	var b strings.Builder
	svgOpen(&b, title)
	drawAxes(&b, yLabel, maxVal, 5)

	n := len(cats)
	if n == 0 {
		n = 1
	}
	slotW := plotW() / float64(n)
	gw := len(groups)
	if gw == 0 {
		gw = 1
	}
	// leave ~22% of the slot as padding between category clusters
	clusterW := slotW * 0.78
	barW := (clusterW - float64(gw-1)*2) / float64(gw)
	if barW < 1 {
		barW = 1
	}
	showLabels := n*gw <= 20

	for ci, cat := range cats {
		slotX := marL + float64(ci)*slotW
		clusterX := slotX + (slotW-clusterW)/2
		for gi, g := range groups {
			if ci >= len(g.values) {
				continue
			}
			v := g.values[ci]
			bh := v / maxVal * plotH()
			x := clusterX + float64(gi)*(barW+2)
			y := marT + plotH() - bh
			tip := fmt.Sprintf("%s — %s: %s", cat, g.name, fmtNum(v))
			fmt.Fprintf(&b, `<path d="%s" fill="%s"><title>%s</title></path>`,
				roundedTopRect(x, y, barW, bh, 3), g.color, html.EscapeString(tip))
			if showLabels && bh > 12 {
				fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" class="vlabel">%s</text>`,
					x+barW/2, y-4, fmtNum(v))
			}
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" class="xlabel">%s</text>`,
			slotX+slotW/2, marT+plotH()+16, html.EscapeString(cat))
	}

	legend(&b, groupLegend(groups))
	b.WriteString("</svg>")
	return b.String()
}

type linePoint struct {
	label string
	y     float64
}

type lineSeries struct {
	name   string
	color  string
	points []linePoint
}

// lineChart renders discrete x levels (evenly spaced by index) with one polyline
// per series. Used for throughput/tail-latency vs parallelism and memory vs N.
func lineChart(title, yLabel string, xLabels []string, series []lineSeries) string {
	var maxVal float64
	for _, s := range series {
		for _, p := range s.points {
			maxVal = max(maxVal, p.y)
		}
	}
	maxVal = niceMax(maxVal)

	var b strings.Builder
	svgOpen(&b, title)
	drawAxes(&b, yLabel, maxVal, 5)

	n := len(xLabels)
	step := plotW()
	if n > 1 {
		step = plotW() / float64(n-1)
	}
	xAt := func(i int) float64 { return marL + float64(i)*step }
	yAt := func(v float64) float64 { return marT + plotH() - v/maxVal*plotH() }

	for i, lbl := range xLabels {
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" class="xlabel">%s</text>`,
			xAt(i), marT+plotH()+16, html.EscapeString(lbl))
	}

	for _, s := range series {
		var pts strings.Builder
		for i, p := range s.points {
			if i > 0 {
				pts.WriteByte(' ')
			}
			fmt.Fprintf(&pts, "%.1f,%.1f", xAt(i), yAt(p.y))
		}
		fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="%s" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/>`,
			pts.String(), s.color)
		for i, p := range s.points {
			tip := fmt.Sprintf("%s @ %s: %s", s.name, p.label, fmtNum(p.y))
			fmt.Fprintf(&b, `<circle cx="%.1f" cy="%.1f" r="4" fill="%s" stroke="var(--surface-1)" stroke-width="1.5"><title>%s</title></circle>`,
				xAt(i), yAt(p.y), s.color, html.EscapeString(tip))
		}
	}

	legend(&b, seriesLegend(series))
	b.WriteString("</svg>")
	return b.String()
}

type stackSeg struct {
	name   string
	color  string
	values []float64 // one per category (x)
}

// stackedBars renders one stacked bar per category, segments in fixed order.
func stackedBars(title, yLabel string, cats []string, segs []stackSeg) string {
	totals := make([]float64, len(cats))
	for _, s := range segs {
		for i, v := range s.values {
			if i < len(totals) {
				totals[i] += v
			}
		}
	}
	var maxVal float64
	if len(totals) > 0 {
		maxVal = slices.Max(totals)
	}
	maxVal = niceMax(maxVal)

	var b strings.Builder
	svgOpen(&b, title)
	drawAxes(&b, yLabel, maxVal, 5)

	n := len(cats)
	if n == 0 {
		n = 1
	}
	slotW := plotW() / float64(n)
	barW := min(slotW*0.5, 90)

	for ci, cat := range cats {
		x := marL + float64(ci)*slotW + (slotW-barW)/2
		var acc float64
		for _, s := range segs {
			if ci >= len(s.values) {
				continue
			}
			v := s.values[ci]
			if v <= 0 {
				continue
			}
			segH := v / maxVal * plotH()
			y := marT + plotH() - (acc+v)/maxVal*plotH()
			// 2px surface gap between segments
			gap := 1.0
			tip := fmt.Sprintf("%s — %s: %s", cat, s.name, fmtNum(v))
			fmt.Fprintf(&b, `<rect x="%.1f" y="%.1f" width="%.1f" height="%.1f" rx="2" fill="%s"><title>%s</title></rect>`,
				x, y+gap, barW, max(segH-2*gap, 0.5), s.color, html.EscapeString(tip))
			acc += v
		}
		fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle" class="xlabel">%s</text>`,
			x+barW/2, marT+plotH()+16, html.EscapeString(cat))
	}

	legend(&b, stackLegend(segs))
	b.WriteString("</svg>")
	return b.String()
}

func svgOpen(b *strings.Builder, title string) {
	fmt.Fprintf(b, `<svg viewBox="0 0 %d %d" role="img" aria-label="%s" class="chart">`,
		chartW, chartH, html.EscapeString(title))
}

func drawAxes(b *strings.Builder, yLabel string, maxVal float64, ticks int) {
	// gridlines + y ticks
	for i := 0; i <= ticks; i++ {
		v := maxVal * float64(i) / float64(ticks)
		y := marT + plotH() - float64(i)/float64(ticks)*plotH()
		fmt.Fprintf(b, `<line x1="%d" y1="%.1f" x2="%.1f" y2="%.1f" class="grid"/>`,
			marL, y, marL+plotW(), y)
		fmt.Fprintf(b, `<text x="%d" y="%.1f" text-anchor="end" class="ytick">%s</text>`,
			marL-8, y+3, fmtNum(v))
	}
	// baseline
	fmt.Fprintf(b, `<line x1="%d" y1="%.1f" x2="%.1f" y2="%.1f" class="axis"/>`,
		marL, marT+plotH(), marL+plotW(), marT+plotH())
	if yLabel != "" {
		fmt.Fprintf(b, `<text x="%d" y="%d" class="axislabel">%s</text>`, 6, marT-12, html.EscapeString(yLabel))
	}
}

type legendItem struct {
	name  string
	color string
}

func groupLegend(groups []barGroup) []legendItem {
	items := make([]legendItem, len(groups))
	for i, g := range groups {
		items[i] = legendItem{g.name, g.color}
	}
	return items
}

func seriesLegend(series []lineSeries) []legendItem {
	items := make([]legendItem, len(series))
	for i, s := range series {
		items[i] = legendItem{s.name, s.color}
	}
	return items
}

func stackLegend(segs []stackSeg) []legendItem {
	items := make([]legendItem, len(segs))
	for i, s := range segs {
		items[i] = legendItem{s.name, s.color}
	}
	return items
}

func legend(b *strings.Builder, items []legendItem) {
	y := float64(chartH - 28)
	x := float64(marL)
	for _, it := range items {
		fmt.Fprintf(b, `<rect x="%.1f" y="%.1f" width="11" height="11" rx="2" fill="%s"/>`, x, y, it.color)
		fmt.Fprintf(b, `<text x="%.1f" y="%.1f" class="legend">%s</text>`, x+16, y+10, html.EscapeString(it.name))
		x += 20 + float64(len(it.name))*7.2 + 16
	}
}

func roundedTopRect(x, y, w, h, r float64) string {
	if h <= 0 {
		h = 0.5
	}
	if r > w/2 {
		r = w / 2
	}
	if r > h {
		r = h
	}
	return fmt.Sprintf("M%.1f %.1f V%.1f Q%.1f %.1f %.1f %.1f H%.1f Q%.1f %.1f %.1f %.1f V%.1f Z",
		x, y+h,
		y+r,
		x, y, x+r, y,
		x+w-r,
		x+w, y, x+w, y+r,
		y+h)
}

func niceMax(v float64) float64 {
	if v <= 0 {
		return 1
	}
	exp := math.Floor(math.Log10(v))
	base := math.Pow(10, exp)
	frac := v / base
	var nice float64
	switch {
	case frac <= 1:
		nice = 1
	case frac <= 2:
		nice = 2
	case frac <= 5:
		nice = 5
	default:
		nice = 10
	}
	return nice * base
}

func fmtNum(v float64) string {
	switch {
	case v == 0:
		return "0"
	case v >= 1000:
		return fmt.Sprintf("%.0f", v)
	case v >= 100:
		return fmt.Sprintf("%.0f", v)
	case v >= 10:
		return fmt.Sprintf("%.1f", v)
	default:
		return fmt.Sprintf("%.2f", v)
	}
}
