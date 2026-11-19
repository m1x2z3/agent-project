package rollout

import (
	"errors"
	"testing"
	"time"

	"agent-project/internal/configregistry"
)

func TestSelectorUsesAllowlistAndStablePercentage(t *testing.T) {
	registry := configregistry.NewRegistry()
	publishAndActivate(t, registry, "v1", time.Now())
	publish(t, registry, "v2", time.Now().Add(time.Second))

	selector, err := NewSelector(registry, RolloutPolicy{
		CandidateVersion: "v2",
		Percentage:       0,
		TenantAllowlist:  map[string]struct{}{"tenant-a": {}},
	})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}

	allowlisted, err := selector.Select(SelectionInput{RequestID: "req-1", TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("allowlisted Select() error = %v", err)
	}
	if !allowlisted.Candidate || allowlisted.ReleaseVersion != "v2" ||
		allowlisted.Reason != "tenant_allowlist" {
		t.Fatalf("allowlisted decision = %+v", allowlisted)
	}

	normal, err := selector.Select(SelectionInput{RequestID: "req-1", TenantID: "tenant-b"})
	if err != nil {
		t.Fatalf("normal Select() error = %v", err)
	}
	if normal.Candidate || normal.ReleaseVersion != "v1" || normal.Reason != "active_version" {
		t.Fatalf("normal decision = %+v", normal)
	}

	repeated, err := selector.Select(SelectionInput{RequestID: "req-1", TenantID: "tenant-b"})
	if err != nil {
		t.Fatalf("repeated Select() error = %v", err)
	}
	if normal != repeated {
		t.Fatalf("selection is not stable: first=%+v repeated=%+v", normal, repeated)
	}
}

func TestSelectorSupportsZeroAndFullTraffic(t *testing.T) {
	registry := configregistry.NewRegistry()
	publishAndActivate(t, registry, "v1", time.Now())
	publish(t, registry, "v2", time.Now().Add(time.Second))

	zero, err := NewSelector(registry, RolloutPolicy{CandidateVersion: "v2"})
	if err != nil {
		t.Fatalf("NewSelector(0%%) error = %v", err)
	}
	decision, err := zero.Select(SelectionInput{RequestID: "req-1"})
	if err != nil {
		t.Fatalf("zero traffic Select() error = %v", err)
	}
	if decision.Candidate {
		t.Fatalf("0%% decision = %+v, want active version", decision)
	}

	full, err := NewSelector(registry, RolloutPolicy{CandidateVersion: "v2", Percentage: 100})
	if err != nil {
		t.Fatalf("NewSelector(100%%) error = %v", err)
	}
	decision, err = full.Select(SelectionInput{RequestID: "req-1"})
	if err != nil {
		t.Fatalf("full traffic Select() error = %v", err)
	}
	if !decision.Candidate || decision.ReleaseVersion != "v2" {
		t.Fatalf("100%% decision = %+v, want candidate", decision)
	}
}

func TestSelectorRejectsUnknownCandidateAndMissingKey(t *testing.T) {
	registry := configregistry.NewRegistry()
	publishAndActivate(t, registry, "v1", time.Now())

	if _, err := NewSelector(registry, RolloutPolicy{CandidateVersion: "missing"}); !errors.Is(err, ErrCandidateNotFound) {
		t.Fatalf("NewSelector() error = %v, want ErrCandidateNotFound", err)
	}
	selector, err := NewSelector(registry, RolloutPolicy{CandidateVersion: "v1"})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	if _, err := selector.Select(SelectionInput{}); !errors.Is(err, ErrSelectionKeyEmpty) {
		t.Fatalf("Select() error = %v, want ErrSelectionKeyEmpty", err)
	}
}

func TestEvaluateGate(t *testing.T) {
	policy := GatePolicy{
		MinSamples:               3,
		MinSuccessRate:           0.9,
		MinStructuredSuccessRate: 0.9,
		MaxP95Latency:            500 * time.Millisecond,
		MaxAverageCost:           0.01,
	}
	tests := []struct {
		name   string
		input  GateMetrics
		action string
		reason string
	}{
		{
			name:   "hold",
			input:  GateMetrics{Samples: 2},
			action: GateHold,
			reason: "insufficient_samples",
		},
		{
			name: "block success rate",
			input: GateMetrics{
				Samples: 3, Successes: 2, StructuredSuccesses: 3,
				Latencies: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
			},
			action: GateBlock,
			reason: "success_rate_below_threshold",
		},
		{
			name: "rollback candidate",
			input: GateMetrics{
				Samples: 3, Latencies: []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond},
			},
			action: GateRollbackCandidate,
			reason: "zero_success_or_structured_rate",
		},
		{
			name: "allow",
			input: GateMetrics{
				Samples: 3, Successes: 3, StructuredSuccesses: 3,
				Latencies: []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond},
				TotalCost: 0.003,
			},
			action: GateAllow,
			reason: "all_gate_metrics_passed",
		},
		{
			name:   "missing latency",
			input:  GateMetrics{Samples: 3, Successes: 3, StructuredSuccesses: 3, TotalCost: 0.003},
			action: GateBlock,
			reason: "latency_samples_missing",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := EvaluateGate(test.input, policy)
			if got.Action != test.action || got.Reason != test.reason {
				t.Fatalf("decision = %+v, want action=%q reason=%q", got, test.action, test.reason)
			}
		})
	}
}

func publishAndActivate(t *testing.T, registry *configregistry.Registry, version string, at time.Time) {
	t.Helper()
	publish(t, registry, version, at)
	if err := registry.Activate(version); err != nil {
		t.Fatalf("Activate(%s) error = %v", version, err)
	}
}

func publish(t *testing.T, registry *configregistry.Registry, version string, at time.Time) {
	t.Helper()
	snapshot := configregistry.ConfigSnapshot{
		ReleaseVersion: version,
		Prompts: map[string]configregistry.PromptVersion{
			"task_analysis": {Name: "task_analysis", Version: "prompt.analysis.v1", Template: "{{.Query}}"},
			"final_answer":  {Name: "final_answer", Version: "prompt.final.v1", Template: "{{.Query}}"},
		},
		Schemas: map[string]configregistry.SchemaVersion{
			"task_analysis": {Kind: "task_analysis", Version: "task_analysis.v1"},
			"final_answer":  {Kind: "final_answer", Version: "final_answer.v1"},
		},
		Routes: map[string]configregistry.ModelRouteVersion{
			"task_analysis": {Task: "task_analysis", Version: "route.analysis.v1", Provider: "fake", Model: "fake-analysis", Timeout: time.Second},
			"final_answer":  {Task: "final_answer", Version: "route.final.v1", Provider: "fake", Model: "fake-final", Timeout: time.Second},
		},
		Pricing: map[string]configregistry.PricingVersion{
			"fake-analysis": {Provider: "fake", Model: "fake-analysis", Version: "price.v1", Currency: "USD"},
			"fake-final":    {Provider: "fake", Model: "fake-final", Version: "price.v1", Currency: "USD"},
		},
		Metadata: configregistry.ReleaseMetadata{
			Version: version, PublishedBy: "test", PublishedAt: at, EffectiveAt: at,
		},
	}
	if err := registry.Publish(snapshot); err != nil {
		t.Fatalf("Publish(%s) error = %v", version, err)
	}
}
