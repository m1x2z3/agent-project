package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-project/internal/configregistry"
	"agent-project/internal/modelgateway"
	"agent-project/internal/runstore"
)

func TestRunStoreFailureContainsOnlyStableMetadata(t *testing.T) {
	failure := RunStoreFailure{
		RunID:     "run-1",
		RequestID: "request-1",
		TenantID:  "tenant-1",
		Operation: "append_event",
		ErrorKind: "invalid",
	}

	if failure.RunID != "run-1" ||
		failure.RequestID != "request-1" ||
		failure.TenantID != "tenant-1" ||
		failure.Operation != "append_event" ||
		failure.ErrorKind != "invalid" {
		t.Fatalf("RunStoreFailure = %+v", failure)
	}
}

func TestClassifyRunStoreError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", err: nil, want: ""},
		{name: "canceled", err: context.Canceled, want: "canceled"},
		{name: "timeout", err: context.DeadlineExceeded, want: "timeout"},
		{name: "invalid run", err: runstore.ErrInvalidRun, want: "invalid"},
		{name: "invalid event", err: runstore.ErrInvalidEvent, want: "invalid"},
		{name: "invalid event type", err: runstore.ErrInvalidEventType, want: "invalid"},
		{name: "already exists", err: runstore.ErrRunAlreadyExists, want: "already_exists"},
		{name: "not found", err: runstore.ErrRunNotFound, want: "not_found"},
		{name: "identity mismatch", err: runstore.ErrRunIdentityMismatch, want: "identity_mismatch"},
		{name: "already finished", err: runstore.ErrRunAlreadyFinished, want: "already_finished"},
		{name: "invalid transition", err: runstore.ErrInvalidStateTransition, want: "invalid_state_transition"},
		{name: "storage error", err: errors.New("sensitive storage message"), want: "storage_error"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyRunStoreError(test.err); got != test.want {
				t.Fatalf("classifyRunStoreError() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestConfigReferencesFromSnapshotKeepsOnlyVersionMetadata(t *testing.T) {
	snapshot := configregistry.ConfigSnapshot{
		ReleaseVersion: "release-v1",
		Prompts: map[string]configregistry.PromptVersion{
			"task_analysis": {
				Name:     "task_analysis",
				Version:  "prompt-v1",
				Template: "敏感 Prompt",
			},
		},
		Schemas: map[string]configregistry.SchemaVersion{
			"task_analysis": {Kind: "task_analysis", Version: "schema-v1"},
		},
		Routes: map[string]configregistry.ModelRouteVersion{
			"task_analysis": {
				Task:     "task_analysis",
				Version:  "route-v1",
				Provider: "deepseek",
				Model:    "deepseek-chat",
			},
		},
		Pricing: map[string]configregistry.PricingVersion{
			"deepseek-chat": {
				Provider: "deepseek",
				Model:    "deepseek-chat",
				Version:  "pricing-v1",
			},
		},
	}

	references := configReferencesFromSnapshot(snapshot)
	if references.ReleaseVersion != "release-v1" ||
		references.PromptVersions["task_analysis"] != "prompt-v1" ||
		references.SchemaVersions["task_analysis"] != "schema-v1" ||
		references.RouteVersions["task_analysis"] != "route-v1" ||
		references.PricingVersions["deepseek-chat"] != "pricing-v1" {
		t.Fatalf("unexpected references: %+v", references)
	}
}

func TestConfigReferencesFromSnapshotDoesNotShareMaps(t *testing.T) {
	snapshot := configregistry.ConfigSnapshot{
		ReleaseVersion: "release-v1",
		Prompts: map[string]configregistry.PromptVersion{
			"task_analysis": {Version: "prompt-v1"},
		},
		Schemas: map[string]configregistry.SchemaVersion{
			"task_analysis": {Version: "schema-v1"},
		},
		Routes: map[string]configregistry.ModelRouteVersion{
			"task_analysis": {Version: "route-v1"},
		},
		Pricing: map[string]configregistry.PricingVersion{
			"deepseek-chat": {Version: "pricing-v1"},
		},
	}

	references := configReferencesFromSnapshot(snapshot)
	references.PromptVersions["task_analysis"] = "changed"
	references.SchemaVersions["task_analysis"] = "changed"
	references.RouteVersions["task_analysis"] = "changed"
	references.PricingVersions["deepseek-chat"] = "changed"

	if snapshot.Prompts["task_analysis"].Version != "prompt-v1" ||
		snapshot.Schemas["task_analysis"].Version != "schema-v1" ||
		snapshot.Routes["task_analysis"].Version != "route-v1" ||
		snapshot.Pricing["deepseek-chat"].Version != "pricing-v1" {
		t.Fatal("config snapshot was mutated through references")
	}
}

func TestRunStoreRecorderStoresOptionalDependencies(t *testing.T) {
	store := &recorderCompileStore{}
	handler := func(RunStoreFailure) {}
	recorder := runStoreRecorder{
		store:   store,
		onError: handler,
	}

	if recorder.store != store {
		t.Fatal("runStoreRecorder did not retain the configured store")
	}
	if recorder.onError == nil {
		t.Fatal("runStoreRecorder did not retain the configured error handler")
	}
}

func TestRunStoreRecorderRecordsRunStarted(t *testing.T) {
	store := &recordingStore{}
	recorder := &runStoreRecorder{store: store}
	startedAt := time.Date(2026, time.November, 25, 3, 30, 0, 123, time.FixedZone("CST", 8*60*60))
	snapshot := configregistry.ConfigSnapshot{
		ReleaseVersion: "release-v1",
		Prompts: map[string]configregistry.PromptVersion{
			"task_analysis": {Version: "prompt-v1", Template: "敏感 Prompt"},
		},
		Schemas: map[string]configregistry.SchemaVersion{
			"task_analysis": {Version: "schema-v1"},
		},
		Routes: map[string]configregistry.ModelRouteVersion{
			"task_analysis": {Version: "route-v1", Provider: "deepseek", Model: "deepseek-chat"},
		},
		Pricing: map[string]configregistry.PricingVersion{
			"deepseek-chat": {Version: "pricing-v1"},
		},
	}
	input := RunInput{
		RequestID: "request-1",
		TenantID:  "tenant-1",
		RunID:     "run-1",
		Query:     "敏感问题",
	}

	recorder.recordRunStarted(context.Background(), input, snapshot, startedAt)

	if store.created.RunID != input.RunID ||
		store.created.RequestID != input.RequestID ||
		store.created.TenantID != input.TenantID ||
		store.created.Status != runstore.RunRunning {
		t.Fatalf("created run = %+v", store.created)
	}
	if !store.created.StartedAt.Equal(startedAt.UTC()) {
		t.Fatalf("started at = %v, want %v", store.created.StartedAt, startedAt.UTC())
	}
	if store.created.Config.ReleaseVersion != "release-v1" ||
		store.created.Config.PromptVersions["task_analysis"] != "prompt-v1" {
		t.Fatalf("created config references = %+v", store.created.Config)
	}
	if store.created.Config.PromptVersions["task_analysis"] == snapshot.Prompts["task_analysis"].Template {
		t.Fatal("created run stored the prompt template instead of its version")
	}
	if len(store.events) != 1 {
		t.Fatalf("events count = %d, want 1", len(store.events))
	}
	event := store.events[0]
	if event.Type != runstore.EventRunStarted ||
		event.Status != string(runstore.RunRunning) ||
		event.RunID != input.RunID ||
		event.RequestID != input.RequestID ||
		event.TenantID != input.TenantID {
		t.Fatalf("started event = %+v", event)
	}
}

func TestRunStoreRecorderNotifiesCreateFailureWithoutAppendingEvent(t *testing.T) {
	store := &recordingStore{createErr: errors.New("sensitive storage error")}
	var failure RunStoreFailure
	recorder := &runStoreRecorder{
		store: store,
		onError: func(got RunStoreFailure) {
			failure = got
		},
	}
	input := RunInput{RequestID: "request-1", TenantID: "tenant-1", RunID: "run-1"}

	recorder.recordRunStarted(context.Background(), input, configregistry.ConfigSnapshot{
		ReleaseVersion: "release-v1",
	}, time.Now())

	if len(store.events) != 0 {
		t.Fatalf("events = %+v, want no event after create failure", store.events)
	}
	if failure.Operation != "create_run" || failure.ErrorKind != "storage_error" {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestRunStoreRecorderNotifiesAppendFailureAfterCreatingRun(t *testing.T) {
	store := &recordingStore{appendErr: errors.New("sensitive storage error")}
	var failure RunStoreFailure
	recorder := &runStoreRecorder{
		store: store,
		onError: func(got RunStoreFailure) {
			failure = got
		},
	}
	input := RunInput{RequestID: "request-1", TenantID: "tenant-1", RunID: "run-1"}

	recorder.recordRunStarted(context.Background(), input, configregistry.ConfigSnapshot{
		ReleaseVersion: "release-v1",
	}, time.Now())

	if store.created.RunID != input.RunID {
		t.Fatalf("created run = %+v", store.created)
	}
	if failure.Operation != "append_event" || failure.ErrorKind != "storage_error" {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestRunStoreRecorderRecordsModelCallWithoutSensitiveContent(t *testing.T) {
	store := &recordingStore{}
	recorder := &runStoreRecorder{store: store}
	input := RunInput{
		RequestID: "request-1",
		TenantID:  "tenant-1",
		RunID:     "run-1",
		Query:     "敏感问题",
	}
	route := configregistry.ModelRouteVersion{
		Provider: "deepseek",
		Model:    "deepseek-chat",
	}
	response := modelgateway.ChatResponse{
		Provider: "deepseek",
		Model:    "deepseek-chat",
		Message: modelgateway.Message{
			Role:    "assistant",
			Content: "完整模型回答和敏感 Prompt",
		},
	}

	recorder.recordModelCall(
		context.Background(),
		input,
		route,
		response,
		125*time.Millisecond,
		nil,
	)

	if len(store.events) != 1 {
		t.Fatalf("events count = %d, want 1", len(store.events))
	}
	event := store.events[0]
	if event.Type != runstore.EventModelCall ||
		event.Provider != "deepseek" ||
		event.Model != "deepseek-chat" ||
		event.Status != "success" ||
		event.DurationMS != 125 ||
		event.ErrorKind != "" {
		t.Fatalf("model event = %+v", event)
	}
	eventText := fmt.Sprintf("%+v", event)
	if strings.Contains(eventText, "完整模型回答") ||
		strings.Contains(eventText, "敏感 Prompt") {
		t.Fatalf("model event contains sensitive content: %s", eventText)
	}
}

func TestRunStoreRecorderRecordsModelCallFailureAsStableMetadata(t *testing.T) {
	store := &recordingStore{}
	recorder := &runStoreRecorder{store: store}
	input := RunInput{RequestID: "request-1", TenantID: "tenant-1", RunID: "run-1"}
	route := configregistry.ModelRouteVersion{Provider: "kimi", Model: "kimi-chat"}

	recorder.recordModelCall(
		context.Background(),
		input,
		route,
		modelgateway.ChatResponse{},
		2*time.Second,
		errors.New("供应商完整错误消息"),
	)

	event := store.events[0]
	if event.Status != "failed" || event.ErrorKind != "model_error" ||
		event.Provider != "kimi" || event.Model != "kimi-chat" ||
		event.DurationMS != 2000 {
		t.Fatalf("model failure event = %+v", event)
	}
	if strings.Contains(fmt.Sprintf("%+v", event), "供应商完整错误消息") {
		t.Fatal("model failure event contains raw provider error")
	}
}

func TestRunStoreRecorderFinishesSuccessfulRun(t *testing.T) {
	store := &recordingStore{}
	recorder := &runStoreRecorder{store: store}
	execution := &runtimeExecution{
		observation: RunObservation{
			Provider:         "deepseek",
			Model:            "deepseek-chat",
			Duration:         1500 * time.Millisecond,
			Success:          true,
			StructuredValid:  true,
			PromptTokens:     4,
			CompletionTokens: 2,
			TotalTokens:      6,
			Cost:             0.01,
		},
	}
	input := RunInput{RequestID: "request-1", TenantID: "tenant-1", RunID: "run-1"}

	recorder.finishRun(context.Background(), input, execution, nil)

	if len(store.events) != 1 || store.events[0].Type != runstore.EventRunSucceeded {
		t.Fatalf("finish events = %+v", store.events)
	}
	if store.events[0].Status != string(runstore.RunSucceeded) ||
		store.events[0].DurationMS != 1500 {
		t.Fatalf("success event = %+v", store.events[0])
	}
	if store.finishedStatus != runstore.RunSucceeded ||
		store.finishedObservation.TotalTokens != 6 ||
		!store.finishedObservation.Success {
		t.Fatalf("finished run = status %q observation %+v",
			store.finishedStatus,
			store.finishedObservation,
		)
	}
}

func TestRunStoreRecorderFinishesFailedAndCanceledRuns(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus runstore.RunStatus
		wantEvent  runstore.EventType
	}{
		{
			name:       "failed",
			err:        errors.New("sensitive runtime error"),
			wantStatus: runstore.RunFailed,
			wantEvent:  runstore.EventRunFailed,
		},
		{
			name:       "canceled",
			err:        context.Canceled,
			wantStatus: runstore.RunCanceled,
			wantEvent:  runstore.EventRunCanceled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &recordingStore{}
			recorder := &runStoreRecorder{store: store}
			execution := &runtimeExecution{
				observation: RunObservation{
					Duration:  20 * time.Millisecond,
					ErrorKind: classifyRuntimeError(test.err),
				},
			}
			input := RunInput{RequestID: "request-1", TenantID: "tenant-1", RunID: "run-1"}

			recorder.finishRun(context.Background(), input, execution, test.err)

			if len(store.events) != 1 || store.events[0].Type != test.wantEvent {
				t.Fatalf("finish events = %+v", store.events)
			}
			if store.finishedStatus != test.wantStatus {
				t.Fatalf("finished status = %q, want %q", store.finishedStatus, test.wantStatus)
			}
			if store.events[0].ErrorKind != classifyRuntimeError(test.err) {
				t.Fatalf("event error kind = %q, want %q",
					store.events[0].ErrorKind,
					classifyRuntimeError(test.err),
				)
			}
		})
	}
}

func TestRunStoreRecorderSkipsShadowRun(t *testing.T) {
	store := &recordingStore{}
	recorder := &runStoreRecorder{store: store}
	execution := &runtimeExecution{shadow: true}

	recorder.finishRun(
		context.Background(),
		RunInput{RequestID: "request-1", TenantID: "tenant-1", RunID: "run-1"},
		execution,
		nil,
	)

	if len(store.events) != 0 || store.finishedStatus != "" {
		t.Fatalf("shadow run was persisted: events=%+v status=%q",
			store.events,
			store.finishedStatus,
		)
	}
}

func TestRunStoreRecorderFinishesCanceledRunWithCanceledBusinessContext(t *testing.T) {
	store := &recordingStore{}
	recorder := &runStoreRecorder{store: store}
	execution := &runtimeExecution{
		observation: RunObservation{
			ErrorKind: "canceled",
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	recorder.finishRun(
		ctx,
		RunInput{RequestID: "request-1", TenantID: "tenant-1", RunID: "run-1"},
		execution,
		context.Canceled,
	)

	if store.finishedStatus != runstore.RunCanceled {
		t.Fatalf("finished status = %q, want %q", store.finishedStatus, runstore.RunCanceled)
	}
	if len(store.events) != 1 || store.events[0].Type != runstore.EventRunCanceled {
		t.Fatalf("canceled events = %+v", store.events)
	}
}

func TestClassifyModelCall(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantState string
		wantKind  string
	}{
		{name: "success", wantState: "success"},
		{name: "canceled", err: context.Canceled, wantState: "canceled", wantKind: "canceled"},
		{name: "timeout", err: context.DeadlineExceeded, wantState: "timeout", wantKind: "timeout"},
		{name: "failed", err: errors.New("provider error"), wantState: "failed", wantKind: "model_error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, kind := classifyModelCall(test.err)
			if status != test.wantState || kind != test.wantKind {
				t.Fatalf("classifyModelCall() = (%q, %q), want (%q, %q)", status, kind, test.wantState, test.wantKind)
			}
		})
	}
}

// recorderCompileStore 仅用于验证适配器可以依赖 runstore.Store，
// 不实现任何实际持久化行为。
type recorderCompileStore struct{}

func (*recorderCompileStore) CreateRun(_ context.Context, _ runstore.RunRecord) error {
	return nil
}

func (*recorderCompileStore) AppendEvent(_ context.Context, _ runstore.RunEvent) error {
	return nil
}

func (*recorderCompileStore) FinishRun(
	_ context.Context,
	_ string,
	_ runstore.RunStatus,
	_ runstore.RunObservation,
) error {
	return nil
}

func (*recorderCompileStore) GetRun(_ context.Context, _ string) (runstore.RunRecord, error) {
	return runstore.RunRecord{}, nil
}

func (*recorderCompileStore) ListEvents(_ context.Context, _ string) ([]runstore.RunEvent, error) {
	return nil, nil
}

type recordingStore struct {
	created             runstore.RunRecord
	events              []runstore.RunEvent
	finishedStatus      runstore.RunStatus
	finishedObservation runstore.RunObservation
	createErr           error
	appendErr           error
}

func (s *recordingStore) CreateRun(_ context.Context, record runstore.RunRecord) error {
	if s.createErr != nil {
		return s.createErr
	}
	s.created = record
	return nil
}

func (s *recordingStore) AppendEvent(_ context.Context, event runstore.RunEvent) error {
	if s.appendErr != nil {
		return s.appendErr
	}
	s.events = append(s.events, event)
	return nil
}

func (s *recordingStore) FinishRun(
	_ context.Context,
	_ string,
	status runstore.RunStatus,
	observation runstore.RunObservation,
) error {
	s.finishedStatus = status
	s.finishedObservation = observation
	return nil
}

func (*recordingStore) GetRun(_ context.Context, _ string) (runstore.RunRecord, error) {
	return runstore.RunRecord{}, nil
}

func (*recordingStore) ListEvents(_ context.Context, _ string) ([]runstore.RunEvent, error) {
	return nil, nil
}
