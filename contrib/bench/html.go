package main

import (
	"fmt"
	"html"
	"os"
	"slices"
	"strconv"
	"strings"
)

func writeHTML(path string, r *Report) error {
	var b strings.Builder
	b.WriteString("<!doctype html><html lang=\"en\"><head><meta charset=\"utf-8\">")
	b.WriteString(`<meta name="viewport" content="width=device-width, initial-scale=1">`)
	b.WriteString("<title>Shim benchmark — systemd vs runc.v2</title>")
	b.WriteString("<style>" + reportCSS + "</style></head><body class=\"viz\">")

	b.WriteString(`<header class="top"><div><h1>containerd shim benchmark</h1>`)
	b.WriteString(`<p class="sub">io.containerd.systemd.v1 vs io.containerd.runc.v2</p></div>`)
	b.WriteString(`<button id="themeToggle" class="toggle" type="button">◐ theme</button></header>`)

	writeProvenance(&b, r)
	writeCharts(&b, r)
	writeDataTable(&b, r)
	writeCaveats(&b)

	b.WriteString(`<script>` + themeJS + `</script>`)
	b.WriteString("</body></html>")

	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func writeProvenance(b *strings.Builder, r *Report) {
	e := r.Env
	rows := [][2]string{
		{"started", r.StartedAt},
		{"host", e.Hostname},
		{"kernel", e.Kernel},
		{"arch", e.Arch},
		{"cpus", strconv.Itoa(e.NumCPU)},
		{"memory", fmtBytes(e.MemTotalBytes)},
		{"cgroup", e.CgroupVersion},
		{"containerd", e.ContainerdVer},
		{"runc", e.RuncVersion},
		{"go", e.GoVersion},
		{"shim commit", short(e.ShimGitCommit, 12)},
		{"inside VM", fmt.Sprintf("%v", e.InsideVM)},
		{"iterations", strconv.Itoa(r.Config.Iterations)},
		{"warmup", strconv.Itoa(r.Config.Warmup)},
		{"parallel", joinInts(r.Config.Parallel)},
		{"scale counts", joinInts(r.Config.ScaleCounts)},
		{"image", r.Config.Image},
	}
	b.WriteString(`<section class="prov"><h2>environment</h2><dl>`)
	for _, kv := range rows {
		if kv[1] == "" {
			continue
		}
		fmt.Fprintf(b, `<div><dt>%s</dt><dd>%s</dd></div>`, html.EscapeString(kv[0]), html.EscapeString(kv[1]))
	}
	b.WriteString(`</dl></section>`)
}

func writeCharts(b *strings.Builder, r *Report) {
	rts := r.runtimesPresent()
	maxP := slices.Max(r.Config.Parallel)

	section := func(title, desc, svg string) {
		if svg == "" {
			return
		}
		fmt.Fprintf(b, `<figure class="card"><figcaption><h3>%s</h3><p>%s</p></figcaption>%s</figure>`,
			html.EscapeString(title), html.EscapeString(desc), svg)
	}

	b.WriteString(`<section class="charts">`)

	// Latency — container & exec per-op, sequential baseline (p=1).
	section("Container run latency (p50, sequential)",
		"Time for each task op across a full container lifecycle. Lower is better.",
		chartPerOp(r, "container", "", 1, []string{"create", "start", "run", "delete", "total"}, "ms"))
	section("Exec latency (p50, sequential)",
		"Time to run a short exec inside a running container.",
		chartPerOp(r, "exec", "spread", 1, []string{"exec_create", "start", "run", "delete", "total"}, "ms"))
	section("Status collection latency (p50, sequential)",
		"State (task status) and Stats (cgroup metrics) call latency.",
		chartPerOp(r, "status", "", 1, []string{"state", "stats"}, "ms"))

	// TTY vs non-TTY.
	section("TTY vs non-TTY total latency (p50, sequential)",
		"The TTY path adds a transient tty unit + PTY-copy helper for the systemd shim.",
		chartTTY(r))

	// Concurrency.
	section("Throughput vs parallelism — container",
		"Container lifecycles completed per second as in-flight concurrency rises.",
		chartVsParallel(r, "container", "", "throughput", "ops/sec"))
	section("Tail latency (p99) vs parallelism — container",
		"p99 of end-to-end container latency under concurrency. Watch the shared-daemon tail.",
		chartVsParallel(r, "container", "", "p99", "ms"))
	section("Tail latency (p99) vs parallelism — exec on one container",
		"Concurrent execs against a single container — worst case for the shared per-container lock.",
		chartVsParallel(r, "exec", "single-container", "p99", "ms"))

	// Footprint.
	section("Shim memory vs container count (peak Pss)",
		"Resident shim memory as containers scale: one shared daemon vs one shim per container.",
		chartScaleMem(r))

	// CPU — fair headline (lifetime-independent) then per-process breakdown.
	section("Server CPU per container (system − harness)",
		"System-wide CPU-seconds over the run minus the harness's own, per container. Lifetime-independent, so short-lived runc is counted — this is the fair CPU comparison.",
		chartAttribCPU(r, "container"))
	section(fmt.Sprintf("CPU by process category — container @ p=%d (breakdown)", maxP),
		"Per-process attribution. Long-lived (shim daemon, PID1, containerd) is exact; short-lived runc / per-container shims are undercounted by sampling — use the fair total above.",
		chartCPUByCategory(r, maxP))
	section("systemd daemon-reload time per container op",
		"Time in systemd daemon-reload (re-parses every unit), which the shim issues on each create and delete. ms per lifecycle.",
		chartReload(r))

	// Shim-internal fallback (systemd only).
	section("State source: reactor vs fallback (systemd shim)",
		"How often exit state came from the in-memory D-Bus reactor vs a systemd GetAll / on-disk fallback.",
		chartFallback(r))

	b.WriteString(`</section>`)
	_ = rts
}

// ---- individual chart builders ----------------------------------------

func chartPerOp(r *Report, scenario, variant string, parallel int, ops []string, yl string) string {
	var groups []barGroup
	any := false
	labels := prettyOps(ops)
	for _, rt := range r.runtimesPresent() {
		s := r.find(scenario, rt, variant, parallel, 0)
		if s == nil {
			continue
		}
		any = true
		vals := make([]float64, len(ops))
		for i, op := range ops {
			vals[i] = p50Op(s, op)
		}
		groups = append(groups, barGroup{rtShort(rt), rtColor(rt), vals})
	}
	if !any {
		return ""
	}
	return groupedBars(scenario, yl, labels, groups)
}

func chartTTY(r *Report) string {
	cats := []string{"container", "container-tty", "exec", "exec-tty"}
	variants := []string{"", "", "spread", "spread"}
	var groups []barGroup
	any := false
	for _, rt := range r.runtimesPresent() {
		vals := make([]float64, len(cats))
		for i, sc := range cats {
			if s := r.find(sc, rt, variants[i], 1, 0); s != nil {
				vals[i] = p50Op(s, "total")
				any = true
			}
		}
		groups = append(groups, barGroup{rtShort(rt), rtColor(rt), vals})
	}
	if !any {
		return ""
	}
	return groupedBars("tty", "ms", cats, groups)
}

func chartVsParallel(r *Report, scenario, variant, metric, yl string) string {
	xs := r.Config.Parallel
	var xlabels []string
	for _, p := range xs {
		xlabels = append(xlabels, "p="+strconv.Itoa(p))
	}
	var series []lineSeries
	any := false
	for _, rt := range r.runtimesPresent() {
		var pts []linePoint
		for _, p := range xs {
			s := r.find(scenario, rt, variant, p, 0)
			var y float64
			switch metric {
			case "throughput":
				if s != nil {
					y = s.ThroughputOpsPerSec
					any = true
				}
			case "p99":
				y = p99Op(s, "total")
				if s != nil {
					any = true
				}
			}
			pts = append(pts, linePoint{label: "p=" + strconv.Itoa(p), y: y})
		}
		series = append(series, lineSeries{rtShort(rt), rtColor(rt), pts})
	}
	if !any {
		return ""
	}
	return lineChart(scenario, yl, xlabels, series)
}

func chartScaleMem(r *Report) string {
	xs := r.Config.ScaleCounts
	if len(xs) == 0 {
		return ""
	}
	var xlabels []string
	for _, n := range xs {
		xlabels = append(xlabels, strconv.Itoa(n))
	}
	shimCat := map[string]string{runtimeSystemd: "shim-systemd", runtimeRunc: "shim-runc"}
	var series []lineSeries
	any := false
	for _, rt := range r.runtimesPresent() {
		cat := shimCat[rt]
		var pts []linePoint
		for _, n := range xs {
			s := r.find("scale", rt, "", 0, n)
			var y float64
			if s != nil {
				y = float64(s.MemPeakPss[cat]) / (1024 * 1024)
				any = true
			}
			pts = append(pts, linePoint{label: strconv.Itoa(n), y: y})
		}
		series = append(series, lineSeries{rtShort(rt), rtColor(rt), pts})
	}
	if !any {
		return ""
	}
	return lineChart("scale", "MB", xlabels, series)
}

func chartCPUByCategory(r *Report, parallel int) string {
	rts := r.runtimesPresent()
	var cats []string
	for _, rt := range rts {
		cats = append(cats, rtShort(rt))
	}
	var segs []stackSeg
	any := false
	for ci, c := range sampledCategories {
		vals := make([]float64, len(rts))
		nonzero := false
		for i, rt := range rts {
			if s := r.find("container", rt, "", parallel, 0); s != nil {
				vals[i] = s.CPUSeconds[string(c)]
				if vals[i] > 0 {
					nonzero = true
					any = true
				}
			}
		}
		if nonzero {
			segs = append(segs, stackSeg{string(c), catColors[ci%len(catColors)], vals})
		}
	}
	if !any {
		return ""
	}
	return stackedBars("cpu", "CPU-seconds", cats, segs)
}

func chartAttribCPU(r *Report, scenario string) string {
	var cats []string
	for _, p := range r.Config.Parallel {
		cats = append(cats, "p="+strconv.Itoa(p))
	}
	var groups []barGroup
	any := false
	for _, rt := range r.runtimesPresent() {
		vals := make([]float64, len(r.Config.Parallel))
		for i, p := range r.Config.Parallel {
			if s := r.find(scenario, rt, "", p, 0); s != nil && s.Iterations > 0 {
				vals[i] = s.AttribCPUSeconds / float64(s.Iterations) * 1000
				any = true
			}
		}
		groups = append(groups, barGroup{rtShort(rt), rtColor(rt), vals})
	}
	if !any {
		return ""
	}
	return groupedBars("cpu/op", "ms/op", cats, groups)
}

func chartReload(r *Report) string {
	order := []string{"container", "exec", "container-tty", "exec-tty", "status", "scale"}
	type agg struct {
		ms    float64
		iters int
	}
	sums := map[string]*agg{}
	for i := range r.Scenarios {
		s := &r.Scenarios[i]
		if s.Runtime != runtimeSystemd || s.ShimVars == nil {
			continue
		}
		a := sums[s.Scenario]
		if a == nil {
			a = &agg{}
			sums[s.Scenario] = a
		}
		a.ms += s.ShimVars.ReloadTotalMs
		a.iters += s.Iterations
	}
	var cats []string
	var vals []float64
	for _, name := range order {
		if a, ok := sums[name]; ok && a.iters > 0 {
			cats = append(cats, name)
			vals = append(vals, a.ms/float64(a.iters))
		}
	}
	if len(cats) == 0 {
		return ""
	}
	return groupedBars("reload", "ms/op", cats, []barGroup{{"systemd daemon-reload", "var(--cat-6)", vals}})
}

func chartFallback(r *Report) string {
	// aggregate systemd shim counters per scenario name
	order := []string{"container", "exec", "exec-tty", "container-tty", "status", "scale"}
	type agg struct{ reactor, getall, ondisk int64 }
	sums := map[string]*agg{}
	for i := range r.Scenarios {
		s := &r.Scenarios[i]
		if s.Runtime != runtimeSystemd || s.ShimVars == nil {
			continue
		}
		a := sums[s.Scenario]
		if a == nil {
			a = &agg{}
			sums[s.Scenario] = a
		}
		a.reactor += s.ShimVars.ReactorHits
		a.getall += s.ShimVars.GetAllFallbacks
		a.ondisk += s.ShimVars.OnDiskReads
	}
	var cats []string
	for _, name := range order {
		if _, ok := sums[name]; ok {
			cats = append(cats, name)
		}
	}
	if len(cats) == 0 {
		return ""
	}
	reactor := make([]float64, len(cats))
	getall := make([]float64, len(cats))
	ondisk := make([]float64, len(cats))
	for i, name := range cats {
		a := sums[name]
		reactor[i] = float64(a.reactor)
		getall[i] = float64(a.getall)
		ondisk[i] = float64(a.ondisk)
	}
	segs := []stackSeg{
		{"reactor hit", "var(--cat-1)", reactor},
		{"GetAll fallback", "var(--cat-6)", getall},
		{"on-disk read", "var(--cat-4)", ondisk},
	}
	return stackedBars("state-source", "events", cats, segs)
}

// ---- data table --------------------------------------------------------

func writeDataTable(b *strings.Builder, r *Report) {
	b.WriteString(`<section class="tablewrap"><h2>all measurements</h2>`)
	b.WriteString(`<p class="sub">Full raw data in <code>results.json</code>, <code>latency.csv</code>, <code>resources.csv</code>.</p>`)
	b.WriteString(`<table><thead><tr>`)
	for _, h := range []string{"scenario", "runtime", "variant", "P", "N", "iters", "err", "thrpt/s", "hdln p50", "hdln p99", "cpu/op ms", "reload/op ms", "shim peak", "hit rate"} {
		fmt.Fprintf(b, `<th>%s</th>`, h)
	}
	b.WriteString(`</tr></thead><tbody>`)
	for i := range r.Scenarios {
		s := &r.Scenarios[i]
		op := headlineOp(s.Scenario)
		hitRate := ""
		reloadPerOp := ""
		if s.ShimVars != nil {
			hitRate = fmt.Sprintf("%.0f%%", s.ShimVars.ReactorHitRate*100)
			if s.Iterations > 0 {
				reloadPerOp = fmt.Sprintf("%.1f", s.ShimVars.ReloadTotalMs/float64(s.Iterations))
			}
		}
		cpuPerOp := ""
		if s.Iterations > 0 {
			cpuPerOp = fmt.Sprintf("%.1f", s.AttribCPUSeconds/float64(s.Iterations)*1000)
		}
		cells := []string{
			s.Scenario, rtShort(s.Runtime), s.Variant,
			itoaz(s.Parallel), itoaz(s.ScaleCount),
			strconv.Itoa(s.Iterations), strconv.Itoa(s.Errors),
			fmt.Sprintf("%.1f", s.ThroughputOpsPerSec),
			fmt.Sprintf("%.2f", p50Op(s, op)),
			fmt.Sprintf("%.2f", p99Op(s, op)),
			cpuPerOp,
			reloadPerOp,
			fmtBytes(shimPeakPss(s)),
			hitRate,
		}
		b.WriteString(`<tr>`)
		for _, c := range cells {
			fmt.Fprintf(b, `<td>%s</td>`, html.EscapeString(c))
		}
		b.WriteString(`</tr>`)
	}
	b.WriteString(`</tbody></table></section>`)
}

func writeCaveats(b *strings.Builder) {
	b.WriteString(`<section class="caveats"><h2>reading these numbers</h2><ul>`)
	for _, c := range []string{
		"systemd shim = one node-wide daemon (+ systemd supervision, + a PTY helper per TTY workload); runc.v2 = one shim process per container. \"shim cpu\"/\"shim peak\" sum the shim-attributable categories for each design.",
		"systemd's own CPU is shared with the rest of the system, so its share is approximate over the measurement window.",
		"Short-lived runc invocations may be undercounted by the sampling interval; both runtimes invoke runc, so this largely cancels in the comparison.",
		"Inside a VM, absolute latency includes virtualization overhead — treat numbers as relative shim-vs-shim, not absolute host performance.",
		"State-source counters are a systemd-shim internal diagnostic (runc.v2 has no equivalent), not a head-to-head metric.",
	} {
		fmt.Fprintf(b, `<li>%s</li>`, html.EscapeString(c))
	}
	b.WriteString(`</ul></section>`)
}

// ---- small helpers -----------------------------------------------------

func headlineOp(scenario string) string {
	switch scenario {
	case "status":
		return "state"
	case "scale":
		return "launch"
	default:
		return "total"
	}
}

func shimPeakPss(s *ScenarioResult) uint64 {
	if s.Runtime == runtimeSystemd {
		return s.MemPeakPss["shim-systemd"] + s.MemPeakPss["tty-helper"]
	}
	return s.MemPeakPss["shim-runc"]
}

func prettyOps(ops []string) []string {
	m := map[string]string{"exec_create": "exec", "container_new": "new", "container_delete": "cdelete"}
	out := make([]string, len(ops))
	for i, o := range ops {
		if p, ok := m[o]; ok {
			out[i] = p
		} else {
			out[i] = o
		}
	}
	return out
}

func itoaz(v int) string {
	if v == 0 {
		return "-"
	}
	return strconv.Itoa(v)
}

func joinInts(v []int) string {
	parts := make([]string, len(v))
	for i, n := range v {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}

func short(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func fmtBytes(b uint64) string {
	switch {
	case b == 0:
		return "0"
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
