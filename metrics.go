package main

import (
	"context"
	"expvar"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
)

// State-source metric keys. These count how the shim answers process
// state/exit queries: from the in-memory D-Bus event reactor (the fast path)
// versus falling back to a systemd GetAll property read or an on-disk exit
// file. The reactor-hit vs getall-fallback ratio is the event reactor's
// effective hit rate; on-disk reads are the other fallback source.
//
// daemon_reload_* measure time spent in systemd `daemon-reload` (Reload over
// D-Bus), which the shim issues on every create/delete because it writes unit
// files to disk. daemon-reload re-parses every unit, so its cost grows with the
// number of units on the host — a prime suspect for create/delete latency.
const (
	metricReactorHits       = "exitstate_reactor_hits"
	metricGetAllFallbacks   = "exitstate_getall_fallbacks"
	metricOnDiskReads       = "state_ondisk_reads"
	metricGetUnitCalls      = "getunitstate_calls"
	metricDaemonReloadCount = "daemon_reload_count"
	metricDaemonReloadNanos = "daemon_reload_nanos"
)

// stateMetrics is published at /debug/vars (wired in serve()). Counters are
// cumulative for the process lifetime; consumers diff two snapshots to get a
// per-window rate. All keys are published up front (as 0) so scrapers see a
// stable schema before the first event arrives.
var stateMetrics = func() *expvar.Map {
	m := expvar.NewMap("shim_state")
	for _, k := range []string{
		metricReactorHits,
		metricGetAllFallbacks,
		metricOnDiskReads,
		metricGetUnitCalls,
		metricDaemonReloadCount,
		metricDaemonReloadNanos,
	} {
		m.Add(k, 0)
	}
	return m
}()

// countState increments one of the state-source counters above.
func countState(key string) { stateMetrics.Add(key, 1) }

// reloadSystemd issues a systemd daemon-reload and records how long it took.
// The count and cumulative duration are exposed via expvar so the benchmark can
// attribute create/delete latency to reload cost. Reloads are timed even on
// error, since the time was still spent.
func reloadSystemd(ctx context.Context, conn *dbus.Conn) error {
	start := time.Now()
	err := conn.ReloadContext(ctx)
	stateMetrics.Add(metricDaemonReloadCount, 1)
	stateMetrics.Add(metricDaemonReloadNanos, int64(time.Since(start)))
	return err
}
