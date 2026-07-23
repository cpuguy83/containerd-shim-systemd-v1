package main

import (
	"context"
	"flag"
	"fmt"
	"iter"
	"log"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("bench: %v", err)
	}
}

func run() error {
	var (
		address     = flag.String("containerd-address", "/run/containerd/containerd.sock", "containerd socket")
		runtimes    = flag.String("runtimes", runtimeSystemd+","+runtimeRunc, "comma-separated runtime names to compare")
		image       = flag.String("image", "docker.io/library/busybox:latest", "OCI image to run")
		namespace   = flag.String("namespace", "bench", "containerd namespace")
		iterations  = flag.Int("iterations", 50, "measured lifecycles per latency scenario cell")
		warmup      = flag.Int("warmup", 5, "warmup lifecycles (discarded) per cell")
		parallel    = flag.String("parallel", "1,4,8,16", "parallelism degrees for latency scenarios")
		scaleCounts = flag.String("scale-counts", "1,5,10,25,50", "container counts for the scale scenario")
		scenarios   = flag.String("scenarios", "container,exec,status,container-tty,exec-tty,scale", "scenarios to run")
		out         = flag.String("out", "", "output dir (default bench-results/<timestamp>)")
		shimVarsURL = flag.String("shim-vars-url", "http://127.0.0.1:8089/debug/vars", "systemd shim expvar endpoint (empty to disable)")
		sampleIval  = flag.Duration("sample-interval", 25*time.Millisecond, "resource sampling interval")
		statusLoops = flag.Int("status-loops", 500, "State+Stats calls per status scenario cell")
		scaleHold   = flag.Duration("scale-hold", 3*time.Second, "steady-state hold before sampling stops in the scale scenario")
	)
	flag.Parse()

	outDir := *out
	if outDir == "" {
		outDir = fmt.Sprintf("bench-results/%s", time.Now().Format("20060102-150405"))
	}

	cfg := Config{
		Address:        *address,
		Runtimes:       slices.Collect(splitCSV(*runtimes)),
		Image:          *image,
		Namespace:      *namespace,
		Iterations:     *iterations,
		Warmup:         *warmup,
		Parallel:       slices.Collect(parseIntList(*parallel)),
		ScaleCounts:    slices.Collect(parseIntList(*scaleCounts)),
		Scenarios:      slices.Collect(splitCSV(*scenarios)),
		Out:            outDir,
		ShimVarsURL:    *shimVarsURL,
		SampleInterval: *sampleIval,
		StatusLoops:    *statusLoops,
		ScaleHold:      *scaleHold,
	}
	if len(cfg.Parallel) == 0 {
		cfg.Parallel = []int{1}
	}

	ctx := namespaces.WithNamespace(context.Background(), cfg.Namespace)

	d, err := newDriver(ctx, cfg)
	if err != nil {
		return err
	}
	defer d.close()

	d.cleanupNamespace(ctx)

	report := Report{
		Env:       gatherEnv(ctx, d),
		StartedAt: time.Now().Format(time.RFC3339),
		Config:    cfg,
	}

	for _, rt := range cfg.Runtimes {
		for _, sc := range cfg.Scenarios {
			results := d.runScenario(ctx, rt, sc)
			report.Scenarios = append(report.Scenarios, results...)
			d.cleanupNamespace(ctx)
			time.Sleep(time.Second) // let the daemon quiesce between cells
		}
	}

	if err := writeOutputs(cfg, &report); err != nil {
		return err
	}
	fmt.Printf("\nWrote results to %s\n  report: %s\n", cfg.Out, cfg.Out+"/report.html")
	return nil
}

// runScenario expands one scenario name into its parallelism/scale cells for a
// runtime and returns the results. Errors are logged and the cell skipped so a
// single failure does not abort the whole run.
func (d *driver) runScenario(ctx context.Context, rt, sc string) []ScenarioResult {
	var results []ScenarioResult
	log.Printf("[%s] scenario %s", rt, sc)

	switch sc {
	case "container":
		for _, p := range d.cfg.Parallel {
			results = append(results, d.scenarioContainer(ctx, rt, false, p))
		}
	case "container-tty":
		for _, p := range d.cfg.Parallel {
			results = append(results, d.scenarioContainer(ctx, rt, true, p))
		}
	case "exec", "exec-tty":
		tty := sc == "exec-tty"
		for _, p := range d.cfg.Parallel {
			if r, err := d.scenarioExec(ctx, rt, tty, p, false); err != nil {
				log.Printf("  %s p=%d spread: %v", sc, p, err)
			} else {
				results = append(results, r)
			}
			if p > 1 {
				if r, err := d.scenarioExec(ctx, rt, tty, p, true); err != nil {
					log.Printf("  %s p=%d single: %v", sc, p, err)
				} else {
					results = append(results, r)
				}
			}
		}
	case "status":
		for _, p := range d.cfg.Parallel {
			if r, err := d.scenarioStatus(ctx, rt, p); err != nil {
				log.Printf("  status p=%d: %v", p, err)
			} else {
				results = append(results, r)
			}
		}
	case "scale":
		for _, n := range d.cfg.ScaleCounts {
			if r, err := d.scenarioScale(ctx, rt, n); err != nil {
				log.Printf("  scale n=%d: %v", n, err)
			} else {
				results = append(results, r)
			}
		}
	default:
		log.Printf("  unknown scenario %q, skipping", sc)
	}
	return results
}

// cleanupNamespace removes any leftover containers/tasks in the bench namespace
// (from this run's teardown races or a prior aborted run).
func (d *driver) cleanupNamespace(ctx context.Context) {
	cs, err := d.client.Containers(ctx)
	if err != nil {
		return
	}
	for _, c := range cs {
		if t, err := c.Task(ctx, nil); err == nil {
			t.Kill(ctx, 9)
			t.Delete(ctx)
		}
		c.Delete(ctx, containerd.WithSnapshotCleanup)
	}
}

func gatherEnv(ctx context.Context, d *driver) Env {
	e := Env{
		Arch:          runtime.GOARCH,
		NumCPU:        runtime.NumCPU(),
		GoVersion:     runtime.Version(),
		ClockTicksHz:  int(userHZ),
		Kernel:        strings.TrimSpace(readFile("/proc/sys/kernel/osrelease")),
		MemTotalBytes: readMemTotal(),
		CgroupVersion: cgroupVersion(),
		RuncVersion:   commandFirstLine("runc", "--version"),
		InsideVM:      detectVM(),
	}
	e.Hostname, _ = os.Hostname()
	if v, err := d.client.Version(ctx); err == nil {
		e.ContainerdVer = v.Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				e.ShimGitCommit = s.Value
			}
		}
	}
	return e
}

func splitCSV(s string) iter.Seq[string] {
	return func(yield func(string) bool) {
		for p := range strings.SplitSeq(s, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if !yield(p) {
				return
			}
		}
	}
}

func parseIntList(s string) iter.Seq[int] {
	return func(yield func(int) bool) {
		for p := range splitCSV(s) {
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				if !yield(n) {
					return
				}
			}
		}
	}
}

func readFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func readMemTotal() uint64 {
	for line := range strings.SplitSeq(readFile("/proc/meminfo"), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			return parseKBLine(line) * 1024
		}
	}
	return 0
}

func cgroupVersion() string {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		return "v2"
	}
	return "v1"
}

func commandFirstLine(name string, args ...string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	out, err := exec.Command(path, args...).Output()
	if err != nil {
		return ""
	}
	if i := strings.IndexByte(string(out), '\n'); i >= 0 {
		return strings.TrimSpace(string(out)[:i])
	}
	return strings.TrimSpace(string(out))
}

func detectVM() bool {
	if path, err := exec.LookPath("systemd-detect-virt"); err == nil {
		out, _ := exec.Command(path, "--vm").Output()
		v := strings.TrimSpace(string(out))
		return v != "" && v != "none"
	}
	return false
}
