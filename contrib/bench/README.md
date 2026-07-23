# Shim benchmark harness

Benchmarks `io.containerd.systemd.v1` against the stock `io.containerd.runc.v2`
shim and renders the results as raw data plus glanceable charts.

It drives real task-API lifecycles through the containerd Go client (the only
thing that differs between runtimes is the `WithRuntime` string), samples
per-process CPU and memory from `/proc`, and — for the systemd shim only —
scrapes the expvar counters at `127.0.0.1:8089/debug/vars` to report how often
exit state was served from the in-memory D-Bus reactor versus a systemd
`GetAll` / on-disk fallback.

## What it measures

- **Latency**, per task op (create / start / run / delete / total for
  containers; exec_create / start / run / delete / total for execs; state /
  stats for status collection), reported as p50/p90/p99/mean/stddev.
- **Concurrency**: every latency scenario runs at each `-parallel` degree, so
  you see throughput and tail latency as in-flight operations rise — the axis
  where one shared daemon (systemd) diverges from one shim per container
  (runc.v2). Concurrent execs also run against a *single* container, the worst
  case for the shared per-container lock.
- **CPU & memory**, attributed by process category (systemd-shim daemon, PTY
  helper, runc.v2 shim, runc, systemd, containerd). Memory is Pss (accounts for
  shared pages).
- **Footprint at scale**: peak shim memory as container count grows.
- **TTY** as a distinct scenario (extra transient tty unit + cgo PTY copier for
  the systemd shim).
- **State-source fallback** (systemd shim internal diagnostic).

## Running it

The harness needs a running containerd with both runtimes registered, root
(reads `/proc` and the root-owned containerd socket), and the image pre-pullable.

### Canonical: the NixOS VM (both runtimes present, isolated, reproducible)

```console
nix run .#vm
# then, at the VM's root console:
bench -runtimes=io.containerd.systemd.v1,io.containerd.runc.v2
```

`bench` is on the VM PATH (built as a subpackage of the flake). Results land in
`bench-results/<timestamp>/` in the VM; copy them out or open `report.html`.

Absolute latency inside a VM includes virtualization overhead — the numbers are
meaningful as *relative* shim-vs-shim comparison.

### Host: `make bench`

Install the systemd shim first (`make test-daemon`, which CI uses), then:

```console
make bench                       # both runtimes, default matrix, under sudo
make bench BENCH_RUNTIMES=io.containerd.runc.v2 BENCH_ITERATIONS=10
```

Override `CONTAINERD_ADDRESS`, `BENCH_RUNTIMES`, `BENCH_ITERATIONS`,
`BENCH_WARMUP`, `BENCH_PARALLEL`, `BENCH_SCALE_COUNTS`, `BENCH_SCENARIOS`,
`BENCH_OUT`, or pass extra flags via `BENCH_FLAGS`.

Run `bench -h` for the full flag list.

## Output

Written to the output dir:

| File | Contents |
|------|----------|
| `report.html` | Self-contained (inline SVG charts + data table), offline, light/dark |
| `results.json` | Full structured results (every cell, op, sample, counter) |
| `latency.csv` | One row per (scenario, runtime, variant, parallel, scale, op) |
| `resources.csv` | Per-cell CPU-seconds, peak Pss, and state-source counters by category |
| `env.json` | Provenance (kernel, containerd/runc versions, cgroup, CPU/mem, commit) |

## Reading the numbers

The systemd shim is one node-wide daemon (plus systemd supervision, plus a PTY
helper per TTY workload); runc.v2 is one shim process per container. The report's
"shim cpu"/"shim peak" columns sum the shim-attributable categories for each
design. systemd's own CPU is shared with the system and therefore approximate;
short-lived runc CPU may be undercounted by the sampling interval but largely
cancels since both runtimes invoke runc. See the caveats section rendered in the
report.
