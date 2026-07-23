package main

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

func writeOutputs(cfg Config, r *Report) error {
	if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(cfg.Out, "results.json"), r); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(cfg.Out, "env.json"), r.Env); err != nil {
		return err
	}
	if err := writeLatencyCSV(filepath.Join(cfg.Out, "latency.csv"), r); err != nil {
		return err
	}
	if err := writeResourceCSV(filepath.Join(cfg.Out, "resources.csv"), r); err != nil {
		return err
	}
	return writeHTML(filepath.Join(cfg.Out, "report.html"), r)
}

func writeJSON(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writeLatencyCSV(path string, r *Report) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	w.Write([]string{"scenario", "runtime", "variant", "parallel", "scale_count", "op",
		"count", "min_ms", "p50_ms", "p90_ms", "p99_ms", "max_ms", "mean_ms", "stddev_ms"})
	for _, s := range r.Scenarios {
		for op, st := range s.Ops {
			w.Write([]string{s.Scenario, s.Runtime, s.Variant,
				strconv.Itoa(s.Parallel), strconv.Itoa(s.ScaleCount), op,
				strconv.Itoa(st.Count),
				f2(st.Min), f2(st.P50), f2(st.P90), f2(st.P99), f2(st.Max), f2(st.Mean), f2(st.Stddev)})
		}
	}
	return w.Error()
}

func writeResourceCSV(path string, r *Report) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()

	header := []string{"scenario", "runtime", "variant", "parallel", "scale_count",
		"iterations", "errors", "wall_s", "throughput_ops_s",
		"system_cpu_s", "harness_cpu_s", "attrib_cpu_s", "system_util_pct", "shim_cgroup_cpu_s"}
	for _, c := range sampledCategories {
		header = append(header, "cpu_s_"+string(c))
	}
	for _, c := range sampledCategories {
		header = append(header, "peak_pss_"+string(c))
	}
	header = append(header, "reactor_hits", "getall_fallbacks", "ondisk_reads", "reactor_hit_rate",
		"daemon_reload_count", "daemon_reload_total_ms", "daemon_reload_mean_ms", "shim_go_heap_bytes")
	w.Write(header)

	for _, s := range r.Scenarios {
		row := []string{s.Scenario, s.Runtime, s.Variant,
			strconv.Itoa(s.Parallel), strconv.Itoa(s.ScaleCount),
			strconv.Itoa(s.Iterations), strconv.Itoa(s.Errors),
			f2(s.WallSeconds), f2(s.ThroughputOpsPerSec),
			f2(s.SystemCPUSeconds), f2(s.HarnessCPUSeconds), f2(s.AttribCPUSeconds),
			f2(s.SystemUtilPct), f2(s.ShimCgroupCPUSecond)}
		for _, c := range sampledCategories {
			row = append(row, f2(s.CPUSeconds[string(c)]))
		}
		for _, c := range sampledCategories {
			row = append(row, strconv.FormatUint(s.MemPeakPss[string(c)], 10))
		}
		if s.ShimVars != nil {
			row = append(row, strconv.FormatInt(s.ShimVars.ReactorHits, 10),
				strconv.FormatInt(s.ShimVars.GetAllFallbacks, 10),
				strconv.FormatInt(s.ShimVars.OnDiskReads, 10),
				f2(s.ShimVars.ReactorHitRate),
				strconv.FormatInt(s.ShimVars.ReloadCount, 10),
				f2(s.ShimVars.ReloadTotalMs),
				f2(s.ShimVars.ReloadMeanMs))
		} else {
			row = append(row, "", "", "", "", "", "", "")
		}
		row = append(row, strconv.FormatUint(s.ShimGoHeapBytes, 10))
		w.Write(row)
	}
	return w.Error()
}

func f2(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }

// ---- lookups over the results -----------------------------------------

func (r *Report) find(scenario, runtime, variant string, parallel, scale int) *ScenarioResult {
	for i := range r.Scenarios {
		s := &r.Scenarios[i]
		if s.Scenario == scenario && s.Runtime == runtime && s.Variant == variant &&
			s.Parallel == parallel && s.ScaleCount == scale {
			return s
		}
	}
	return nil
}

func (r *Report) runtimesPresent() []string {
	seen := map[string]bool{}
	var out []string
	for _, rt := range r.Config.Runtimes {
		if !seen[rt] {
			for _, s := range r.Scenarios {
				if s.Runtime == rt {
					out = append(out, rt)
					seen[rt] = true
					break
				}
			}
		}
	}
	return out
}

func rtColor(rt string) string {
	switch rt {
	case runtimeSystemd:
		return "var(--rt-systemd)"
	case runtimeRunc:
		return "var(--rt-runc)"
	default:
		return "var(--cat-3)"
	}
}

func rtShort(rt string) string {
	switch rt {
	case runtimeSystemd:
		return "systemd"
	case runtimeRunc:
		return "runc.v2"
	default:
		return rt
	}
}

func p50Op(s *ScenarioResult, op string) float64 {
	if s == nil {
		return 0
	}
	return s.Ops[op].P50
}

func p99Op(s *ScenarioResult, op string) float64 {
	if s == nil {
		return 0
	}
	return s.Ops[op].P99
}
