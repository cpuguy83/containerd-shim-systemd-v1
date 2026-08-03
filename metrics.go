package main

import (
	"expvar"
)

// State-source metric keys. These count how the shim resolves a process's exit:
// from the in-memory D-Bus event reactor (the fast path) versus having to read
// the unit's properties from systemd. The reactor-hit vs getall-fallback ratio
// is the event reactor's effective hit rate.
const (
	metricReactorHits     = "exitstate_reactor_hits"
	metricGetAllFallbacks = "exitstate_getall_fallbacks"
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
		metricGetUnitCalls,
	} {
		m.Add(k, 0)
	}
	return m
}()

// countState increments one of the state-source counters above.
func countState(key string) { stateMetrics.Add(key, 1) }
