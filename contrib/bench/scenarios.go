package main

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
)

const statusPoolSize = 10

// measuredOut bundles everything captured while running a measured op batch.
type measuredOut struct {
	raw    map[string][]float64
	errs   int
	wall   time.Duration
	count  int
	sample sampleResult
	vars   *ShimVarsDelta
	goHeap uint64
}

// measure runs warmup (discarded) then `total` measured invocations of opFn at
// the given parallelism, wrapping the measured region with resource sampling
// and (for the systemd shim) a before/after scrape of the expvar counters.
func (d *driver) measure(ctx context.Context, rt string, total, parallel int, opFn func() (map[string]float64, error)) measuredOut {
	if d.cfg.Warmup > 0 {
		runConcurrentN(d.cfg.Warmup, parallel, opFn)
	}

	scrape := rt == runtimeSystemd && d.cfg.ShimVarsURL != ""
	var before *ShimVarsSnapshot
	if scrape {
		before, _ = scrapeShimVars(ctx, d.cfg.ShimVarsURL)
	}

	smp := newSampler(d.cfg.SampleInterval)
	smp.start()
	raw, errs, wall := runConcurrentN(total, parallel, opFn)
	sres := smp.stopAndCollect()

	out := measuredOut{raw: raw, errs: errs, wall: wall, count: total, sample: sres}
	if scrape {
		after, _ := scrapeShimVars(ctx, d.cfg.ShimVarsURL)
		out.vars = diffShimVars(before, after)
		if after != nil {
			out.goHeap = after.GoHeapAlloc
		}
	}
	return out
}

func buildResult(scenario, rt, variant string, parallel int, m measuredOut) ScenarioResult {
	r := ScenarioResult{
		Scenario:        scenario,
		Runtime:         rt,
		Variant:         variant,
		Parallel:        parallel,
		Iterations:      m.count,
		Errors:          m.errs,
		WallSeconds:     m.wall.Seconds(),
		Ops:             map[string]OpStats{},
		CPUSeconds:      convertCatMapToStringMap(m.sample.cpuSeconds),
		MemPeakPss:      convertCatMapToStringMap(m.sample.peakPss),
		MemMeanPss:      convertCatMapToStringMap(m.sample.meanPss),
		Series:          m.sample.series,
		ShimVars:        m.vars,
		ShimGoHeapBytes: m.goHeap,
	}
	r.SystemCPUSeconds = m.sample.sysBusySeconds
	r.HarnessCPUSeconds = m.sample.harnessSeconds
	r.AttribCPUSeconds = m.sample.sysBusySeconds - m.sample.harnessSeconds
	if r.AttribCPUSeconds < 0 {
		r.AttribCPUSeconds = 0
	}
	if m.sample.sysTotalSeconds > 0 {
		r.SystemUtilPct = m.sample.sysBusySeconds / m.sample.sysTotalSeconds * 100
	}
	r.ShimCgroupCPUSecond = m.sample.shimCgroupSeconds
	for k, v := range m.raw {
		r.Ops[k] = summarize(v, true)
	}
	if m.wall > 0 {
		r.ThroughputOpsPerSec = float64(m.count) / m.wall.Seconds()
	}
	return r
}

// scenarioContainer times full container lifecycles at a parallelism degree.
func (d *driver) scenarioContainer(ctx context.Context, rt string, tty bool, parallel int) ScenarioResult {
	opFn := func() (map[string]float64, error) { return d.containerLifecycle(ctx, rt, tty) }
	m := d.measure(ctx, rt, d.cfg.Iterations, parallel, opFn)
	name := "container"
	if tty {
		name = "container-tty"
	}
	return buildResult(name, rt, "", parallel, m)
}

// scenarioExec times short execs against running containers. With single=true
// all execs target one container (worst case for the shared per-container
// lock); otherwise they spread across `parallel` containers.
func (d *driver) scenarioExec(ctx context.Context, rt string, tty bool, parallel int, single bool) (ScenarioResult, error) {
	nContainers := parallel
	variant := "spread"
	if single {
		nContainers = 1
		variant = "single-container"
	}
	if nContainers < 1 {
		nContainers = 1
	}

	tasks, teardown, err := d.taskPool(ctx, rt, "execbase", nContainers)
	if err != nil {
		return ScenarioResult{}, err
	}
	defer teardown()

	var rr atomic.Int64
	opFn := func() (map[string]float64, error) {
		task := tasks[int(rr.Add(1))%len(tasks)]
		return d.execOnce(ctx, task, tty)
	}
	m := d.measure(ctx, rt, d.cfg.Iterations, parallel, opFn)
	name := "exec"
	if tty {
		name = "exec-tty"
	}
	return buildResult(name, rt, variant, parallel, m), nil
}

// scenarioStatus times State (task.Status) and Stats (task.Metrics) collection
// across a pool of running containers under P concurrent workers.
func (d *driver) scenarioStatus(ctx context.Context, rt string, parallel int) (ScenarioResult, error) {
	tasks, teardown, err := d.taskPool(ctx, rt, "statusbase", statusPoolSize)
	if err != nil {
		return ScenarioResult{}, err
	}
	defer teardown()

	var rr atomic.Int64
	opFn := func() (map[string]float64, error) {
		task := tasks[int(rr.Add(1))%len(tasks)]
		ops := map[string]float64{}
		t0 := time.Now()
		_, err := task.Status(ctx)
		ops["state"] = msSince(t0)
		if err != nil {
			return ops, err
		}
		t1 := time.Now()
		_, err = task.Metrics(ctx)
		ops["stats"] = msSince(t1)
		if err != nil {
			return ops, err
		}
		return ops, nil
	}
	m := d.measure(ctx, rt, d.cfg.StatusLoops, parallel, opFn)
	return buildResult("status", rt, "", parallel, m), nil
}

// scenarioScale brings up `count` long-running containers concurrently, holds
// them at steady state while sampling aggregate resource use, then tears them
// down. This is the 1-daemon-vs-N-shims footprint measurement.
func (d *driver) scenarioScale(ctx context.Context, rt string, count int) (ScenarioResult, error) {
	maxPar := max(1, slices.Max(d.cfg.Parallel))

	scrape := rt == runtimeSystemd && d.cfg.ShimVarsURL != ""
	var before *ShimVarsSnapshot
	if scrape {
		before, _ = scrapeShimVars(ctx, d.cfg.ShimVarsURL)
	}

	smp := newSampler(d.cfg.SampleInterval)
	smp.start()

	var mu sync.Mutex
	var teardowns []func()
	launchFn := func() (map[string]float64, error) {
		t0 := time.Now()
		task, td, err := d.longContainer(ctx, rt, d.uid("scale"))
		dur := msSince(t0)
		if err != nil {
			return map[string]float64{"launch": dur}, err
		}
		_ = task
		mu.Lock()
		teardowns = append(teardowns, td)
		mu.Unlock()
		return map[string]float64{"launch": dur}, nil
	}

	raw, errs, wall := runConcurrentN(count, maxPar, launchFn)
	if d.cfg.ScaleHold > 0 {
		time.Sleep(d.cfg.ScaleHold)
	}
	sres := smp.stopAndCollect()

	var vars *ShimVarsDelta
	var goHeap uint64
	if scrape {
		after, _ := scrapeShimVars(ctx, d.cfg.ShimVarsURL)
		vars = diffShimVars(before, after)
		if after != nil {
			goHeap = after.GoHeapAlloc
		}
	}

	mu.Lock()
	tds := teardowns
	mu.Unlock()
	for _, td := range tds {
		td()
	}

	m := measuredOut{raw: raw, errs: errs, wall: wall, count: count, sample: sres, vars: vars, goHeap: goHeap}
	r := buildResult("scale", rt, "", 0, m)
	r.ScaleCount = count
	if wall > 0 {
		r.ThroughputOpsPerSec = float64(count-errs) / wall.Seconds()
	}
	return r, nil
}

// taskPool creates n running containers and returns their tasks plus a single
// teardown func that cleans them all up.
func (d *driver) taskPool(ctx context.Context, rt, prefix string, n int) ([]containerd.Task, func(), error) {
	var tasks []containerd.Task
	var teardowns []func()
	cleanup := func() {
		for _, td := range teardowns {
			td()
		}
	}
	for range n {
		task, td, err := d.longContainer(ctx, rt, d.uid(prefix))
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		tasks = append(tasks, task)
		teardowns = append(teardowns, td)
	}
	return tasks, cleanup, nil
}

func convertCatMapToStringMap[T any](input map[category]T) map[string]T {
	out := make(map[string]T)
	for k, v := range input {
		out[string(k)] = v
	}
	return out
}
