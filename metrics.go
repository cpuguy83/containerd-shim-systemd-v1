package main

import (
	"expvar"
)

// State-source metric keys. These count how the shim answers process
// state/exit queries: from the in-memory D-Bus event reactor (the fast path)
// versus falling back to a systemd GetAll property read or an on-disk exit
// file. The reactor-hit vs getall-fallback ratio is the event reactor's
// effective hit rate; on-disk reads are the other fallback source.
const (
	metricReactorHits     = "exitstate_reactor_hits"
	metricGetAllFallbacks = "exitstate_getall_fallbacks"
	metricOnDiskReads     = "state_ondisk_reads"
	metricGetUnitCalls    = "getunitstate_calls"
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
	} {
		m.Add(k, 0)
	}
	return m
}()

// countState increments one of the state-source counters above.
func countState(key string) { stateMetrics.Add(key, 1) }
