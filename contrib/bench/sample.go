package main

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// userHZ is Linux USER_HZ (CLK_TCK). It is 100 on effectively all Linux
// configurations; /proc/<pid>/stat CPU fields are counted in these ticks.
const userHZ = 100.0

type category string

const (
	catShimSystemd category = "shim-systemd"
	catTTYHelper   category = "tty-helper"
	catShimRunc    category = "shim-runc"
	catRunc        category = "runc"
	catSystemd     category = "systemd"
	catContainerd  category = "containerd"
	catOther       category = ""
)

// sampledCategories is the fixed set reported for every scenario so charts and
// CSVs have a stable schema.
var sampledCategories = []category{
	catShimSystemd, catTTYHelper, catShimRunc, catRunc, catSystemd, catContainerd,
}

// sampler periodically walks /proc to attribute CPU and Pss to process
// categories over a measurement window. CPU is accounted exactly for
// long-lived processes (the shim daemon, containerd) and approximately for
// short-lived ones (runc invocations may be undercounted at coarse intervals);
// see the plan's fairness caveats.
type sampler struct {
	interval    time.Duration
	seriesEvery time.Duration

	stop chan struct{}
	done chan struct{}

	startWall time.Time

	// baseline holds cumulative CPU-seconds at first observation for pids that
	// already existed when sampling started; their window contribution is
	// (current - baseline). Pids first seen later were born during the window,
	// so their full cumulative counts.
	baseline map[int]float64
	contrib  map[int]float64
	catOf    map[int]category

	pssTotals map[category][]uint64

	series       []SamplePoint
	lastSeries   time.Time
	lastContrib  map[category]float64
	lastSeriesOK bool

	// Window-level, lifetime-independent CPU accounting (start/end deltas), so
	// short-lived processes (runc, per-container shims) are not undercounted the
	// way high-frequency per-process sampling undercounts them.
	sysBusyStart  float64 // /proc/stat busy jiffies
	sysTotalStart float64 // /proc/stat total jiffies
	selfStart     float64 // this process (driver + sampler) CPU seconds
	shimCgStart   int64   // systemd shim daemon cgroup usage_usec (-1 if n/a)
}

// shimCgroupCPUStat is the systemd shim daemon's cgroup v2 cpu accounting file
// (it runs as a system service). Used for an exact cross-check of the daemon's
// CPU independent of sampling.
const shimCgroupCPUStat = "/sys/fs/cgroup/system.slice/containerd-shim-systemd-v1.service/cpu.stat"

func newSampler(interval time.Duration) *sampler {
	if interval <= 0 {
		interval = 25 * time.Millisecond
	}
	return &sampler{
		interval:    interval,
		seriesEvery: max(5*interval, 200*time.Millisecond),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		baseline:    map[int]float64{},
		contrib:     map[int]float64{},
		catOf:       map[int]category{},
		pssTotals:   map[category][]uint64{},
		lastContrib: map[category]float64{},
	}
}

func (s *sampler) start() {
	s.startWall = time.Now()
	s.sysBusyStart, s.sysTotalStart = readSystemCPU()
	s.selfStart = readSelfCPU()
	s.shimCgStart = readCgroupUsageUsec(shimCgroupCPUStat)
	go s.run()
}

func (s *sampler) run() {
	defer close(s.done)
	s.sampleOnce(true)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			s.sampleOnce(false)
			return
		case <-t.C:
			s.sampleOnce(false)
		}
	}
}

// stopAndCollect stops sampling and returns the aggregated result.
func (s *sampler) stopAndCollect() sampleResult {
	close(s.stop)
	<-s.done

	busyEnd, totalEnd := readSystemCPU()
	selfEnd := readSelfCPU()
	shimCgEnd := readCgroupUsageUsec(shimCgroupCPUStat)

	res := s.collect()
	res.sysBusySeconds = (busyEnd - s.sysBusyStart) / userHZ
	res.sysTotalSeconds = (totalEnd - s.sysTotalStart) / userHZ
	res.harnessSeconds = (selfEnd - s.selfStart) / userHZ
	res.shimCgroupSeconds = -1
	if s.shimCgStart >= 0 && shimCgEnd >= 0 {
		res.shimCgroupSeconds = float64(shimCgEnd-s.shimCgStart) / 1e6
	}
	return res
}

type sampleResult struct {
	cpuSeconds map[category]float64
	peakPss    map[category]uint64
	meanPss    map[category]uint64
	series     []SamplePoint
	wall       float64

	sysBusySeconds    float64 // system-wide busy CPU-seconds over the window
	sysTotalSeconds   float64 // system-wide total CPU-seconds (busy+idle), all cores
	harnessSeconds    float64 // this process's CPU (driver + sampler) — subtract for server-side
	shimCgroupSeconds float64 // systemd shim daemon cgroup CPU (-1 if unavailable)
}

func (s *sampler) collect() sampleResult {
	res := sampleResult{
		cpuSeconds: map[category]float64{},
		peakPss:    map[category]uint64{},
		meanPss:    map[category]uint64{},
		series:     s.series,
		wall:       time.Since(s.startWall).Seconds(),
	}
	for pid, c := range s.catOf {
		res.cpuSeconds[c] += s.contrib[pid]
	}
	for _, c := range sampledCategories {
		vals := s.pssTotals[c]
		if len(vals) == 0 {
			continue
		}
		var sum, peak uint64
		for _, v := range vals {
			sum += v
			peak = max(peak, v)
		}
		res.peakPss[c] = peak
		res.meanPss[c] = sum / uint64(len(vals))
	}
	return res
}

func (s *sampler) sampleOnce(baselinePass bool) {
	now := time.Now()
	perCatPss := map[category]uint64{}

	pids, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, ent := range pids {
		if !ent.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		argv := readCmdline(pid)
		comm, cpuTicks, ok := readStat(pid)
		if !ok {
			continue
		}
		cat := categorize(pid, argv, comm)
		if cat == catOther {
			continue
		}
		cpuCum := cpuTicks / userHZ

		if baselinePass {
			s.baseline[pid] = cpuCum
			s.contrib[pid] = 0
		} else if b, existed := s.baseline[pid]; existed {
			if d := cpuCum - b; d >= 0 {
				s.contrib[pid] = d
			}
		} else {
			s.contrib[pid] = cpuCum
		}
		s.catOf[pid] = cat

		perCatPss[cat] += readPss(pid)
	}

	for _, c := range sampledCategories {
		s.pssTotals[c] = append(s.pssTotals[c], perCatPss[c])
	}

	if baselinePass {
		s.lastSeries = now
		return
	}
	if now.Sub(s.lastSeries) < s.seriesEvery {
		return
	}
	s.emitSeries(now, perCatPss)
}

func (s *sampler) emitSeries(now time.Time, perCatPss map[category]uint64) {
	nowContrib := map[category]float64{}
	for pid, c := range s.catOf {
		nowContrib[c] += s.contrib[pid]
	}
	pt := SamplePoint{
		TSeconds: now.Sub(s.startWall).Seconds(),
		PssBytes: map[string]uint64{},
		CPUPct:   map[string]float64{},
	}
	elapsed := now.Sub(s.lastSeries).Seconds()
	for _, c := range sampledCategories {
		pt.PssBytes[string(c)] = perCatPss[c]
		if s.lastSeriesOK && elapsed > 0 {
			pt.CPUPct[string(c)] = (nowContrib[c] - s.lastContrib[c]) / elapsed * 100
		}
	}
	s.series = append(s.series, pt)
	s.lastContrib = nowContrib
	s.lastSeries = now
	s.lastSeriesOK = true
}

// categorize maps a process to a benchmark category using its argv (comm is
// truncated to 15 chars, so both shims share the comm "containerd-shim" and
// cannot be told apart without the full command line).
func categorize(pid int, argv []string, comm string) category {
	base := comm
	if len(argv) > 0 && argv[0] != "" {
		base = filepath.Base(argv[0])
	}
	switch base {
	case "containerd-shim-systemd-v1":
		if slices.Contains(argv, "tty-handshake") {
			return catTTYHelper
		}
		return catShimSystemd
	case "containerd-shim-runc-v2":
		return catShimRunc
	case "runc":
		return catRunc
	case "containerd":
		return catContainerd
	case "systemd":
		return catSystemd
	}
	if pid == 1 {
		return catSystemd
	}
	return catOther
}

func readCmdline(pid int) []string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil || len(data) == 0 {
		return nil
	}
	parts := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	return parts
}

// readStat returns the process comm and cumulative (utime+stime) in ticks.
func readStat(pid int) (comm string, cpuTicks float64, ok bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", 0, false
	}
	s := string(data)
	// comm is wrapped in parens and may itself contain spaces/parens; split on
	// the final ')' so the numeric fields after it parse cleanly.
	lp := strings.IndexByte(s, '(')
	rp := strings.LastIndexByte(s, ')')
	if lp < 0 || rp < 0 || rp < lp {
		return "", 0, false
	}
	comm = s[lp+1 : rp]
	rest := strings.Fields(s[rp+1:])
	// rest[0]=state, so utime (field 14) is rest[11] and stime (field 15) rest[12].
	if len(rest) < 13 {
		return comm, 0, false
	}
	utime, err1 := strconv.ParseFloat(rest[11], 64)
	stime, err2 := strconv.ParseFloat(rest[12], 64)
	if err1 != nil || err2 != nil {
		return comm, 0, false
	}
	return comm, utime + stime, true
}

// readPss returns the process proportional set size in bytes, preferring
// smaps_rollup (accounts for shared pages) and falling back to VmRSS.
func readPss(pid int) uint64 {
	if data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/smaps_rollup"); err == nil {
		for line := range strings.SplitSeq(string(data), "\n") {
			if strings.HasPrefix(line, "Pss:") {
				return parseKBLine(line) * 1024
			}
		}
	}
	if data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status"); err == nil {
		for line := range strings.SplitSeq(string(data), "\n") {
			if strings.HasPrefix(line, "VmRSS:") {
				return parseKBLine(line) * 1024
			}
		}
	}
	return 0
}

func parseKBLine(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	v, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// readSystemCPU returns aggregate busy and total CPU jiffies from /proc/stat's
// "cpu" line. Busy excludes idle+iowait. This captures all CPU regardless of
// process lifetime, so short-lived runc/shim processes are counted.
func readSystemCPU() (busy, total float64) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:]
		var idle float64
		for i, f := range fields {
			v, err := strconv.ParseFloat(f, 64)
			if err != nil {
				continue
			}
			total += v
			if i == 3 || i == 4 { // idle, iowait
				idle += v
			}
		}
		busy = total - idle
		return busy, total
	}
	return 0, 0
}

// readSelfCPU returns this process's cumulative CPU-seconds (driver + sampler).
func readSelfCPU() float64 {
	_, ticks, ok := readStat(os.Getpid())
	if !ok {
		return 0
	}
	return ticks / userHZ
}

// readCgroupUsageUsec reads usage_usec from a cgroup v2 cpu.stat file, or -1.
func readCgroupUsageUsec(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "usage_usec") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
					return v
				}
			}
		}
	}
	return -1
}
