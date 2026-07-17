// Copyright 2026 SandrPod Contributors
// Prometheus-compatible /metrics endpoint. Dependency-free: it renders the
// text exposition format directly from the current store snapshot, so there is
// no client library or background collector to maintain.

package main

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	podpkg "github.com/sandrpod/sandrpod/pkg/sandpod"
)

// metricsHandler returns a handler that renders current counts from the stores.
func metricsHandler(stores *podpkg.Stores) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder

		// Sandboxes by state.
		sbByState := map[string]int{}
		for _, sb := range stores.Sandboxes.List() {
			sbByState[string(sb.State)]++
		}
		b.WriteString("# HELP sandrpod_sandboxes Number of sandboxes by state.\n")
		b.WriteString("# TYPE sandrpod_sandboxes gauge\n")
		writeLabeled(&b, "sandrpod_sandboxes", "state", sbByState)

		// Poders by state, plus aggregate capacity/usage.
		poders := stores.Poders.List()
		podByState := map[string]int{}
		var capContainers, usedContainers int
		for _, p := range poders {
			podByState[string(p.State)]++
			if p.State == podpkg.PoderStateOnline {
				capContainers += p.Resources.MaxContainers
				usedContainers += p.Usage.Containers
			}
		}
		b.WriteString("# HELP sandrpod_poders Number of poders by state.\n")
		b.WriteString("# TYPE sandrpod_poders gauge\n")
		writeLabeled(&b, "sandrpod_poders", "state", podByState)

		writeScalar(&b, "sandrpod_poder_container_capacity",
			"Total container slots across ONLINE poders.", float64(capContainers))
		writeScalar(&b, "sandrpod_poder_containers_in_use",
			"Containers currently running across ONLINE poders.", float64(usedContainers))

		// Jobs by status.
		jobByStatus := map[string]int{}
		for _, j := range stores.Jobs.ListJobs() {
			jobByStatus[string(j.Status)]++
		}
		b.WriteString("# HELP sandrpod_jobs Number of jobs by status.\n")
		b.WriteString("# TYPE sandrpod_jobs gauge\n")
		writeLabeled(&b, "sandrpod_jobs", "status", jobByStatus)

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Write([]byte(b.String()))
	}
}

// writeLabeled emits one metric line per label value, sorted for stable output.
func writeLabeled(b *strings.Builder, metric, label string, values map[string]int) {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "%s{%s=%q} %d\n", metric, label, k, values[k])
	}
}

// writeScalar emits a single HELP/TYPE/value triple.
func writeScalar(b *strings.Builder, metric, help string, v float64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", metric, help, metric, metric, v)
}
