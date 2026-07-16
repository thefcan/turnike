package metrics

import (
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestNewExposesExactlyFourFamilies pins the whole surface: the four
// M5 families and nothing else - in particular no go_* / process_*
// collectors sneaking in via a default registry.
func TestNewExposesExactlyFourFamilies(t *testing.T) {
	m := New()
	m.InitRoute("/api/")
	fams, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var got []string
	for _, f := range fams {
		got = append(got, f.GetName())
	}
	want := []string{
		"turnike_breaker_state",
		"turnike_limiter_backend",
		"turnike_request_duration_seconds",
		"turnike_requests_total",
	}
	if !slices.Equal(got, want) { // Gather returns families name-sorted
		t.Fatalf("families = %v, want %v", got, want)
	}
}

// TestLabelNamesAreBounded is the structural pin on the cardinality
// rule: every label name comes from the fixed {route, decision,
// backend} vocabulary, so client identity (key, api_key, ip) can never
// become a label without this test failing.
func TestLabelNamesAreBounded(t *testing.T) {
	allowed := map[string]bool{"route": true, "decision": true, "backend": true}
	m := New()
	m.InitRoute("/api/")
	m.SetActiveBackend(BackendRedis)
	fams, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range fams {
		for _, mt := range f.GetMetric() {
			for _, lp := range mt.GetLabel() {
				if !allowed[lp.GetName()] {
					t.Errorf("family %s carries label %q; labels must stay in the bounded vocabulary", f.GetName(), lp.GetName())
				}
			}
		}
	}
}

// TestSetActiveBackendFlips asserts the sequential one-hot semantics:
// after each call exactly the named backend reads 1. (Concurrent
// interleavings are documented as last-decision-wins and not asserted
// here - that would pin a guarantee the two independent Sets don't
// make.)
func TestSetActiveBackendFlips(t *testing.T) {
	m := New()
	read := func(b string) float64 {
		return testutil.ToFloat64(m.LimiterBackend.WithLabelValues(b))
	}
	if r, mem := read(BackendRedis), read(BackendMemory); r != 0 || mem != 0 {
		t.Fatalf("before any decision: redis=%v memory=%v, want both 0", r, mem)
	}
	m.SetActiveBackend(BackendRedis)
	if r, mem := read(BackendRedis), read(BackendMemory); r != 1 || mem != 0 {
		t.Fatalf("after redis: redis=%v memory=%v, want 1/0", r, mem)
	}
	m.SetActiveBackend(BackendMemory)
	if r, mem := read(BackendRedis), read(BackendMemory); r != 0 || mem != 1 {
		t.Fatalf("after memory: redis=%v memory=%v, want 0/1", r, mem)
	}
}

// TestInitRouteMaterializesEveryDecision checks that a configured
// route surfaces all five decision series at 0 immediately, so rate()
// never misses a series' first increment.
func TestInitRouteMaterializesEveryDecision(t *testing.T) {
	m := New()
	m.InitRoute("/x/")
	fams, err := m.Registry.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != "turnike_requests_total" {
			continue
		}
		var got []string
		for _, mt := range f.GetMetric() {
			if v := mt.GetCounter().GetValue(); v != 0 {
				t.Errorf("pre-materialized series has value %v, want 0", v)
			}
			for _, lp := range mt.GetLabel() {
				switch lp.GetName() {
				case "decision":
					got = append(got, lp.GetValue())
				case "route":
					if lp.GetValue() != "/x/" {
						t.Errorf("route = %q, want /x/", lp.GetValue())
					}
				}
			}
		}
		slices.Sort(got)
		want := []string{DecisionAllow, DecisionDegrade, DecisionDegradeAllow, DecisionDegradeDeny, DecisionDeny}
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("decision series = %v, want %v", got, want)
		}
		return
	}
	t.Fatal("turnike_requests_total not gathered")
}
