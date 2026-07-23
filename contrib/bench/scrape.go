package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ShimVarsSnapshot is a point-in-time read of the systemd shim's expvar
// counters (published at /debug/vars). runc.v2 has no such endpoint, so these
// are shim-internal diagnostics rather than a head-to-head comparison metric.
type ShimVarsSnapshot struct {
	ReactorHits     int64
	GetAllFallbacks int64
	OnDiskReads     int64
	GetUnitCalls    int64
	ReloadCount     int64
	ReloadNanos     int64
	GoHeapAlloc     uint64
}

// ShimVarsDelta is the difference between two snapshots over a scenario window.
type ShimVarsDelta struct {
	ReactorHits     int64   `json:"reactor_hits"`
	GetAllFallbacks int64   `json:"getall_fallbacks"`
	OnDiskReads     int64   `json:"ondisk_reads"`
	GetUnitCalls    int64   `json:"getunitstate_calls"`
	ReactorHitRate  float64 `json:"reactor_hit_rate"`
	ReloadCount     int64   `json:"daemon_reload_count"`
	ReloadTotalMs   float64 `json:"daemon_reload_total_ms"`
	ReloadMeanMs    float64 `json:"daemon_reload_mean_ms"`
}

// scrapeShimVars fetches and parses the shim's expvar endpoint. It returns
// (nil, nil) when url is empty so callers can treat "no endpoint" and "runc
// runtime" uniformly.
func scrapeShimVars(ctx context.Context, url string) (*ShimVarsSnapshot, error) {
	if url == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("shim vars: unexpected status %s", resp.Status)
	}

	var raw struct {
		ShimState struct {
			ReactorHits int64 `json:"exitstate_reactor_hits"`
			GetAll      int64 `json:"exitstate_getall_fallbacks"`
			OnDisk      int64 `json:"state_ondisk_reads"`
			GetUnit     int64 `json:"getunitstate_calls"`
			ReloadCount int64 `json:"daemon_reload_count"`
			ReloadNanos int64 `json:"daemon_reload_nanos"`
		} `json:"shim_state"`
		Memstats struct {
			Alloc uint64 `json:"Alloc"`
		} `json:"memstats"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	return &ShimVarsSnapshot{
		ReactorHits:     raw.ShimState.ReactorHits,
		GetAllFallbacks: raw.ShimState.GetAll,
		OnDiskReads:     raw.ShimState.OnDisk,
		GetUnitCalls:    raw.ShimState.GetUnit,
		ReloadCount:     raw.ShimState.ReloadCount,
		ReloadNanos:     raw.ShimState.ReloadNanos,
		GoHeapAlloc:     raw.Memstats.Alloc,
	}, nil
}

// diffShimVars computes before→after deltas and the reactor hit rate.
func diffShimVars(before, after *ShimVarsSnapshot) *ShimVarsDelta {
	if before == nil || after == nil {
		return nil
	}
	d := &ShimVarsDelta{
		ReactorHits:     after.ReactorHits - before.ReactorHits,
		GetAllFallbacks: after.GetAllFallbacks - before.GetAllFallbacks,
		OnDiskReads:     after.OnDiskReads - before.OnDiskReads,
		GetUnitCalls:    after.GetUnitCalls - before.GetUnitCalls,
		ReloadCount:     after.ReloadCount - before.ReloadCount,
		ReloadTotalMs:   float64(after.ReloadNanos-before.ReloadNanos) / 1e6,
	}
	if total := d.ReactorHits + d.GetAllFallbacks; total > 0 {
		d.ReactorHitRate = float64(d.ReactorHits) / float64(total)
	}
	if d.ReloadCount > 0 {
		d.ReloadMeanMs = d.ReloadTotalMs / float64(d.ReloadCount)
	}
	return d
}
