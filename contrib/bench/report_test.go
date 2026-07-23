package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReportOutputs(t *testing.T) {
	// given: a synthetic run with both runtimes across latency, tty, status,
	// and scale scenarios, with resource and state-source data populated.
	r := syntheticReport()
	dir := t.TempDir()
	cfg := r.Config
	cfg.Out = dir

	// when: the outputs are written.
	if err := writeOutputs(cfg, &r); err != nil {
		t.Fatalf("writeOutputs: %v", err)
	}

	t.Run("all artifacts are written", func(t *testing.T) {
		for _, name := range []string{"results.json", "env.json", "latency.csv", "resources.csv", "report.html"} {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Errorf("missing %s: %v", name, err)
			}
		}
	})

	htmlBytes, err := os.ReadFile(filepath.Join(dir, "report.html"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(htmlBytes)

	// Optionally dump the artifact for manual inspection.
	if out := os.Getenv("BENCH_DUMP"); out != "" {
		os.WriteFile(out, htmlBytes, 0o644)
	}

	t.Run("html has no unfilled format directives", func(t *testing.T) {
		if strings.Contains(doc, "%!") {
			t.Errorf("report.html contains a Go format error (%%!)")
		}
	})

	t.Run("every svg element is closed", func(t *testing.T) {
		if o, c := strings.Count(doc, "<svg"), strings.Count(doc, "</svg>"); o != c {
			t.Errorf("unbalanced svg tags: %d open, %d close", o, c)
		}
		if o, c := strings.Count(doc, "<path"), strings.Count(doc, "</path>")+strings.Count(doc, "/>"); o > c {
			t.Errorf("suspiciously many unclosed path tags: %d open", o)
		}
	})

	t.Run("charts and palette are present", func(t *testing.T) {
		for _, marker := range []string{"--rt-systemd", "--cat-1", "class=\"chart\"", "systemd", "runc.v2"} {
			if !strings.Contains(doc, marker) {
				t.Errorf("report.html missing marker %q", marker)
			}
		}
		if n := strings.Count(doc, "<svg"); n < 5 {
			t.Errorf("expected several charts, found %d svg elements", n)
		}
	})

	t.Run("no NaN or Inf leaked into svg coordinates", func(t *testing.T) {
		for _, bad := range []string{"NaN", "+Inf", "-Inf"} {
			if strings.Contains(doc, bad) {
				t.Errorf("report.html contains %q", bad)
			}
		}
	})

	t.Run("csv rows are emitted for measurements", func(t *testing.T) {
		lat, _ := os.ReadFile(filepath.Join(dir, "latency.csv"))
		if lines := strings.Count(string(lat), "\n"); lines < 5 {
			t.Errorf("latency.csv looks empty: %d lines", lines)
		}
		res, _ := os.ReadFile(filepath.Join(dir, "resources.csv"))
		if !strings.Contains(string(res), "reactor_hit_rate") {
			t.Errorf("resources.csv missing expected header")
		}
	})
}

// syntheticReport builds a deterministic Report covering the chart code paths.
func syntheticReport() Report {
	cfg := Config{
		Runtimes:    []string{runtimeSystemd, runtimeRunc},
		Image:       "docker.io/library/busybox:latest",
		Iterations:  50,
		Warmup:      5,
		Parallel:    []int{1, 4},
		ScaleCounts: []int{1, 5},
	}
	r := Report{
		Env: Env{
			Hostname: "test", Kernel: "7.1.4", Arch: "amd64", NumCPU: 8,
			MemTotalBytes: 16 << 30, CgroupVersion: "v2", GoVersion: "go1.26",
			ContainerdVer: "2.3.3", RuncVersion: "runc 1.2", InsideVM: true,
		},
		StartedAt: "2026-07-22T12:00:00Z",
		Config:    cfg,
	}

	mkOps := func(names []string, base float64) map[string]OpStats {
		m := map[string]OpStats{}
		for i, n := range names {
			v := base + float64(i)*2
			m[n] = OpStats{Count: 50, Min: v * 0.8, P50: v, P90: v * 1.4, P99: v * 2, Max: v * 3, Mean: v * 1.1, Stddev: v * 0.2}
		}
		return m
	}
	cpu := func(shim, runcS float64) map[string]float64 {
		return map[string]float64{"shim-systemd": shim, "tty-helper": 0, "shim-runc": runcS, "runc": 1.2, "systemd": 0.3, "containerd": 0.5}
	}
	mem := func(shim uint64) map[string]uint64 {
		return map[string]uint64{"shim-systemd": shim, "tty-helper": 0, "shim-runc": shim, "runc": 4 << 20, "systemd": 20 << 20, "containerd": 30 << 20}
	}
	vars := func(hit, getall, ondisk int64) *ShimVarsDelta {
		d := &ShimVarsDelta{ReactorHits: hit, GetAllFallbacks: getall, OnDiskReads: ondisk,
			ReloadCount: 40, ReloadTotalMs: 400, ReloadMeanMs: 10}
		if hit+getall > 0 {
			d.ReactorHitRate = float64(hit) / float64(hit+getall)
		}
		return d
	}

	add := func(s ScenarioResult) { r.Scenarios = append(r.Scenarios, s) }

	for ri, rt := range cfg.Runtimes {
		sysd := rt == runtimeSystemd
		base := 20.0 + float64(ri)*5
		for _, p := range cfg.Parallel {
			c := ScenarioResult{
				Scenario: "container", Runtime: rt, Parallel: p, Iterations: 50,
				ThroughputOpsPerSec: 30 - float64(ri*5) - float64(p), WallSeconds: 2,
				Ops:        mkOps([]string{"create", "start", "run", "delete", "total"}, base+float64(p)),
				CPUSeconds: cpu(1.5, 2.0), MemPeakPss: mem(uint64(40+ri*10) << 20),
				SystemCPUSeconds: 8 - float64(ri)*3, HarnessCPUSeconds: 1.5,
				AttribCPUSeconds: 6.5 - float64(ri)*3, SystemUtilPct: 40 - float64(ri)*10,
			}
			if sysd {
				c.ShimVars = vars(int64(90+p), int64(10), int64(2))
				c.ShimGoHeapBytes = 12 << 20
			}
			add(c)

			e := ScenarioResult{
				Scenario: "exec", Runtime: rt, Variant: "spread", Parallel: p, Iterations: 50,
				ThroughputOpsPerSec: 40 - float64(p), WallSeconds: 1.5,
				Ops:        mkOps([]string{"exec_create", "start", "run", "delete", "total"}, base*0.6),
				CPUSeconds: cpu(0.8, 1.1), MemPeakPss: mem(uint64(38+ri*8) << 20),
			}
			if sysd {
				e.ShimVars = vars(int64(80), int64(5), int64(1))
			}
			add(e)

			if p > 1 {
				es := e
				es.Variant = "single-container"
				es.Ops = mkOps([]string{"exec_create", "start", "run", "delete", "total"}, base*0.6+float64(p)*3)
				add(es)
			}
		}

		st := ScenarioResult{
			Scenario: "status", Runtime: rt, Parallel: 1, Iterations: 500,
			Ops: mkOps([]string{"state", "stats"}, 1.5), CPUSeconds: cpu(0.2, 0.3), MemPeakPss: mem(uint64(35+ri*8) << 20),
		}
		add(st)

		ct := ScenarioResult{
			Scenario: "container-tty", Runtime: rt, Parallel: 1, Iterations: 50,
			Ops: mkOps([]string{"create", "start", "run", "delete", "total"}, base*1.3), CPUSeconds: cpu(1.8, 2.1), MemPeakPss: mem(uint64(45+ri*10) << 20),
		}
		add(ct)
		et := ScenarioResult{
			Scenario: "exec-tty", Runtime: rt, Variant: "spread", Parallel: 1, Iterations: 50,
			Ops: mkOps([]string{"exec_create", "start", "run", "delete", "total"}, base*0.8), CPUSeconds: cpu(1.0, 1.3), MemPeakPss: mem(uint64(42+ri*9) << 20),
		}
		add(et)

		for _, n := range cfg.ScaleCounts {
			sc := ScenarioResult{
				Scenario: "scale", Runtime: rt, ScaleCount: n, Iterations: n,
				ThroughputOpsPerSec: 15 - float64(ri*3), WallSeconds: float64(n) * 0.1,
				Ops:        map[string]OpStats{"launch": {Count: n, P50: 60 + base, P99: 120}},
				CPUSeconds: cpu(float64(n)*0.1, float64(n)*0.2),
				MemPeakPss: mem(uint64((30+ri*5)+n*(2+ri*3)) << 20),
			}
			if sysd {
				sc.ShimVars = vars(int64(n*2), int64(1), 0)
			}
			add(sc)
		}
	}
	return r
}
