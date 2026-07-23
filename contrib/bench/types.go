package main

import "time"

// runtime identifiers under comparison.
const (
	runtimeSystemd = "io.containerd.systemd.v1"
	runtimeRunc    = "io.containerd.runc.v2"
)

// Config is the fully-resolved benchmark configuration for a run.
type Config struct {
	Address        string        `json:"address"`
	Runtimes       []string      `json:"runtimes"`
	Image          string        `json:"image"`
	Namespace      string        `json:"namespace"`
	Iterations     int           `json:"iterations"`
	Warmup         int           `json:"warmup"`
	Parallel       []int         `json:"parallel"`
	ScaleCounts    []int         `json:"scale_counts"`
	Scenarios      []string      `json:"scenarios"`
	Out            string        `json:"out"`
	ShimVarsURL    string        `json:"shim_vars_url"`
	SampleInterval time.Duration `json:"sample_interval_ns"`
	StatusLoops    int           `json:"status_loops"`
	ScaleHold      time.Duration `json:"scale_hold_ns"`
}

// Report is the top-level output written to results.json.
type Report struct {
	Env       Env              `json:"env"`
	StartedAt string           `json:"started_at"`
	Config    Config           `json:"config"`
	Scenarios []ScenarioResult `json:"scenarios"`
}

// Env captures provenance so a results file is self-describing.
type Env struct {
	Hostname      string `json:"hostname"`
	Kernel        string `json:"kernel"`
	Arch          string `json:"arch"`
	NumCPU        int    `json:"num_cpu"`
	MemTotalBytes uint64 `json:"mem_total_bytes"`
	CgroupVersion string `json:"cgroup_version"`
	GoVersion     string `json:"go_version"`
	ContainerdVer string `json:"containerd_version"`
	RuncVersion   string `json:"runc_version"`
	ShimGitCommit string `json:"shim_git_commit"`
	ClockTicksHz  int    `json:"clock_ticks_hz"`
	InsideVM      bool   `json:"inside_vm"`
}

// ScenarioResult holds one measured (scenario, runtime, parallelism|scale) cell.
type ScenarioResult struct {
	Scenario            string             `json:"scenario"`
	Runtime             string             `json:"runtime"`
	Variant             string             `json:"variant,omitempty"`
	Parallel            int                `json:"parallel,omitempty"`
	ScaleCount          int                `json:"scale_count,omitempty"`
	Iterations          int                `json:"iterations"`
	Errors              int                `json:"errors"`
	WallSeconds         float64            `json:"wall_seconds"`
	ThroughputOpsPerSec float64            `json:"throughput_ops_per_sec,omitempty"`
	Ops                 map[string]OpStats `json:"ops,omitempty"`
	CPUSeconds          map[string]float64 `json:"cpu_seconds,omitempty"`
	SystemCPUSeconds    float64            `json:"system_cpu_seconds"`
	HarnessCPUSeconds   float64            `json:"harness_cpu_seconds"`
	AttribCPUSeconds    float64            `json:"attrib_cpu_seconds"`
	SystemUtilPct       float64            `json:"system_util_pct"`
	ShimCgroupCPUSecond float64            `json:"shim_cgroup_cpu_seconds,omitempty"`
	MemPeakPss          map[string]uint64  `json:"mem_peak_pss_bytes,omitempty"`
	MemMeanPss          map[string]uint64  `json:"mem_mean_pss_bytes,omitempty"`
	ShimVars            *ShimVarsDelta     `json:"shim_vars_delta,omitempty"`
	ShimGoHeapBytes     uint64             `json:"shim_go_heap_bytes,omitempty"`
	Series              []SamplePoint      `json:"series,omitempty"`
}

// OpStats is the latency distribution for a single named operation, in ms.
type OpStats struct {
	Count  int       `json:"count"`
	Min    float64   `json:"min_ms"`
	P50    float64   `json:"p50_ms"`
	P90    float64   `json:"p90_ms"`
	P99    float64   `json:"p99_ms"`
	Max    float64   `json:"max_ms"`
	Mean   float64   `json:"mean_ms"`
	Stddev float64   `json:"stddev_ms"`
	Raw    []float64 `json:"raw_ms,omitempty"`
}

// SamplePoint is one downsampled point in a scenario's resource time series.
type SamplePoint struct {
	TSeconds float64            `json:"t_seconds"`
	PssBytes map[string]uint64  `json:"pss_bytes"`
	CPUPct   map[string]float64 `json:"cpu_pct"`
}
