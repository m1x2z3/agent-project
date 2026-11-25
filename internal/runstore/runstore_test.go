package runstore

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunstoreErrorCategoriesAreStable(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "invalid run", err: ErrInvalidRun},
		{name: "run already exists", err: ErrRunAlreadyExists},
		{name: "run not found", err: ErrRunNotFound},
		{name: "invalid event", err: ErrInvalidEvent},
		{name: "invalid event type", err: ErrInvalidEventType},
		{name: "run identity mismatch", err: ErrRunIdentityMismatch},
		{name: "run already finished", err: ErrRunAlreadyFinished},
		{name: "invalid state transition", err: ErrInvalidStateTransition},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.err == nil {
				t.Fatal("error category must not be nil")
			}
			if !errors.Is(test.err, test.err) {
				t.Fatalf("errors.Is() does not match %v", test.err)
			}
		})
	}
}

func TestRunstoreErrorCategoriesHaveDistinctMessages(t *testing.T) {
	errorsByMessage := make(map[string]struct{})
	for _, err := range []error{
		ErrInvalidRun,
		ErrRunAlreadyExists,
		ErrRunNotFound,
		ErrInvalidEvent,
		ErrInvalidEventType,
		ErrRunIdentityMismatch,
		ErrRunAlreadyFinished,
		ErrInvalidStateTransition,
	} {
		message := err.Error()
		if _, exists := errorsByMessage[message]; exists {
			t.Fatalf("duplicate error message %q", message)
		}
		errorsByMessage[message] = struct{}{}
	}
}

func TestEventTypeValuesAreStableAndUnique(t *testing.T) {
	tests := []struct {
		name  string
		event EventType
		want  EventType
	}{
		{name: "run started", event: EventRunStarted, want: "run.started"},
		{name: "model call", event: EventModelCall, want: "model.call"},
		{name: "tool call", event: EventToolCall, want: "tool.call"},
		{name: "tool result", event: EventToolResult, want: "tool.result"},
		{name: "run succeeded", event: EventRunSucceeded, want: "run.succeeded"},
		{name: "run failed", event: EventRunFailed, want: "run.failed"},
	}

	seen := make(map[EventType]struct{}, len(tests))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.event != test.want {
				t.Fatalf("event type = %q, want %q", test.event, test.want)
			}
			if test.event == "" {
				t.Fatal("event type must not be empty")
			}
			if _, exists := seen[test.event]; exists {
				t.Fatalf("duplicate event type %q", test.event)
			}
			seen[test.event] = struct{}{}
		})
	}
}

func TestRunEventStoresRedactedExecutionMetadata(t *testing.T) {
	occurredAt := time.Date(2026, time.November, 25, 9, 30, 0, 0, time.UTC)
	event := RunEvent{
		RunID:           "run-1",
		RequestID:       "request-1",
		TenantID:        "tenant-1",
		Type:            EventToolResult,
		OccurredAt:      occurredAt,
		Provider:        "deepseek",
		Model:           "deepseek-chat",
		ToolName:        "search_project_docs",
		ToolVersion:     "v1",
		Status:          "success",
		DurationMS:      12,
		ArgumentsDigest: "arguments-digest",
		ResultDigest:    "result-digest",
	}

	if event.Sequence != 0 {
		t.Fatalf("sequence = %d, want storage-assigned zero value", event.Sequence)
	}
	if event.Type != EventToolResult {
		t.Fatalf("event type = %q, want %q", event.Type, EventToolResult)
	}
	if !event.OccurredAt.Equal(occurredAt) {
		t.Fatalf("occurred at = %v, want %v", event.OccurredAt, occurredAt)
	}
	if event.ToolName == "" || event.ToolVersion == "" {
		t.Fatalf("tool metadata = %+v", event)
	}
	if event.ArgumentsDigest == "" || event.ResultDigest == "" {
		t.Fatalf("event digests = %+v", event)
	}
}

func TestRunEventCanRepresentModelFailure(t *testing.T) {
	event := RunEvent{
		RunID:     "run-1",
		Type:      EventModelCall,
		Status:    "failed",
		ErrorKind: "timeout",
	}

	if event.Type != EventModelCall {
		t.Fatalf("event type = %q, want %q", event.Type, EventModelCall)
	}
	if event.Status != "failed" || event.ErrorKind != "timeout" {
		t.Fatalf("failure event = %+v", event)
	}
}

func TestRunStatusValuesAreStable(t *testing.T) {
	tests := []struct {
		name   string
		status RunStatus
		want   RunStatus
	}{
		{name: "running", status: RunRunning, want: "running"},
		{name: "succeeded", status: RunSucceeded, want: "succeeded"},
		{name: "failed", status: RunFailed, want: "failed"},
		{name: "canceled", status: RunCanceled, want: "canceled"},
	}

	seen := make(map[RunStatus]struct{}, len(tests))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.status != test.want {
				t.Fatalf("status = %q, want %q", test.status, test.want)
			}
			if test.status == "" {
				t.Fatal("status must not be empty")
			}
			if _, exists := seen[test.status]; exists {
				t.Fatalf("duplicate status %q", test.status)
			}
			seen[test.status] = struct{}{}
		})
	}
}

func TestConfigReferencesStoresVersionReferencesOnly(t *testing.T) {
	references := ConfigReferences{
		ReleaseVersion: "release-v1",
		PromptVersions: map[string]string{
			"task_analysis": "prompt-v1",
		},
		SchemaVersions: map[string]string{
			"task_analysis": "schema-v1",
		},
		RouteVersions: map[string]string{
			"task_analysis": "route-v1",
		},
		PricingVersions: map[string]string{
			"deepseek:chat": "pricing-v1",
		},
	}

	if references.ReleaseVersion == "" {
		t.Fatal("release version must not be empty")
	}
	if references.PromptVersions["task_analysis"] != "prompt-v1" {
		t.Fatalf("prompt version = %q, want %q",
			references.PromptVersions["task_analysis"], "prompt-v1")
	}
	if references.SchemaVersions["task_analysis"] != "schema-v1" {
		t.Fatalf("schema version = %q, want %q",
			references.SchemaVersions["task_analysis"], "schema-v1")
	}
	if references.RouteVersions["task_analysis"] != "route-v1" {
		t.Fatalf("route version = %q, want %q",
			references.RouteVersions["task_analysis"], "route-v1")
	}
	if references.PricingVersions["deepseek:chat"] != "pricing-v1" {
		t.Fatalf("pricing version = %q, want %q",
			references.PricingVersions["deepseek:chat"], "pricing-v1")
	}
}

func TestRunObservationStoresSanitizedRuntimeMetadata(t *testing.T) {
	observation := RunObservation{
		Provider:         "deepseek",
		Model:            "deepseek-chat",
		Duration:         120 * time.Millisecond,
		Success:          true,
		StructuredValid:  true,
		PromptTokens:     80,
		CompletionTokens: 40,
		TotalTokens:      120,
		Cost:             0.003,
	}

	if observation.Provider != "deepseek" || observation.Model != "deepseek-chat" {
		t.Fatalf("model metadata = %+v", observation)
	}
	if observation.Duration != 120*time.Millisecond {
		t.Fatalf("duration = %v, want %v", observation.Duration, 120*time.Millisecond)
	}
	if observation.TotalTokens != observation.PromptTokens+observation.CompletionTokens {
		t.Fatalf("total tokens = %d, want prompt + completion = %d",
			observation.TotalTokens,
			observation.PromptTokens+observation.CompletionTokens)
	}
	if !observation.Success || !observation.StructuredValid {
		t.Fatalf("successful observation = %+v", observation)
	}
}

func TestRunObservationCanRepresentFailureMetadata(t *testing.T) {
	observation := RunObservation{
		Provider:  "kimi",
		Model:     "moonshot-v1",
		Duration:  2 * time.Second,
		ErrorKind: "timeout",
	}

	if observation.Success {
		t.Fatal("failed observation must not be marked successful")
	}
	if observation.ErrorKind != "timeout" {
		t.Fatalf("error kind = %q, want timeout", observation.ErrorKind)
	}
}

func TestRunRecordCombinesIdentityLifecycleConfigAndObservation(t *testing.T) {
	startedAt := time.Date(2026, time.November, 24, 10, 0, 0, 0, time.UTC)
	record := RunRecord{
		RunID:     "run-1",
		RequestID: "request-1",
		TenantID:  "tenant-1",
		Status:    RunRunning,
		Config: ConfigReferences{
			ReleaseVersion: "release-v1",
		},
		StartedAt: startedAt,
		Observation: RunObservation{
			Provider: "deepseek",
			Model:    "deepseek-chat",
		},
	}

	if record.RunID == "" || record.RequestID == "" || record.TenantID == "" {
		t.Fatalf("identity fields = %+v", record)
	}
	if record.Status != RunRunning {
		t.Fatalf("status = %q, want %q", record.Status, RunRunning)
	}
	if record.Config.ReleaseVersion != "release-v1" {
		t.Fatalf("release version = %q, want release-v1", record.Config.ReleaseVersion)
	}
	if !record.StartedAt.Equal(startedAt) {
		t.Fatalf("started at = %v, want %v", record.StartedAt, startedAt)
	}
	if record.Observation.Provider != "deepseek" {
		t.Fatalf("observation = %+v", record.Observation)
	}
	if !record.FinishedAt.IsZero() {
		t.Fatalf("running record finished at %v", record.FinishedAt)
	}
}

type compileOnlyStore struct{}

func (compileOnlyStore) CreateRun(context.Context, RunRecord) error {
	return nil
}

func (compileOnlyStore) AppendEvent(context.Context, RunEvent) error {
	return nil
}

func (compileOnlyStore) FinishRun(context.Context, string, RunStatus, RunObservation) error {
	return nil
}

func (compileOnlyStore) GetRun(context.Context, string) (RunRecord, error) {
	return RunRecord{}, nil
}

func (compileOnlyStore) ListEvents(context.Context, string) ([]RunEvent, error) {
	return nil, nil
}

var _ Store = compileOnlyStore{}

func TestStoreInterfaceDefinesRunLifecycleBoundary(t *testing.T) {
	var store Store = compileOnlyStore{}
	if store == nil {
		t.Fatal("store interface must accept a compatible implementation")
	}
}
