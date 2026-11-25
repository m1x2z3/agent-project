package memory

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"agent-project/internal/runstore"
)

func TestMemoryStoreKeepsIndependentRunAndEventState(t *testing.T) {
	store := MemoryStore{
		runs: map[string]runstore.RunRecord{
			"run-1": {RunID: "run-1"},
		},
		events: map[string][]runstore.RunEvent{
			"run-1": {{RunID: "run-1", Type: runstore.EventRunStarted}},
		},
		nextSequence: map[string]uint64{
			"run-1": 1,
		},
	}

	if _, ok := store.runs["run-1"]; !ok {
		t.Fatal("run state is missing")
	}
	if len(store.events["run-1"]) != 1 {
		t.Fatalf("event count = %d, want 1", len(store.events["run-1"]))
	}
	if store.nextSequence["run-1"] != 1 {
		t.Fatalf("next sequence = %d, want 1", store.nextSequence["run-1"])
	}
}

func TestNewMemoryStoreInitializesStateMaps(t *testing.T) {
	store := NewMemoryStore()
	if store == nil {
		t.Fatal("NewMemoryStore() returned nil")
	}
	if store.runs == nil || store.events == nil || store.nextSequence == nil {
		t.Fatal("NewMemoryStore() did not initialize all state maps")
	}
	if len(store.runs) != 0 || len(store.events) != 0 || len(store.nextSequence) != 0 {
		t.Fatal("NewMemoryStore() returned a non-empty store")
	}
}

func TestMemoryStoreCreateRun(t *testing.T) {
	store := NewMemoryStore()
	record := runstore.RunRecord{
		RunID:     "run-1",
		RequestID: "request-1",
		TenantID:  "tenant-1",
		Status:    runstore.RunRunning,
		Config: runstore.ConfigReferences{
			ReleaseVersion: "release-v1",
			PromptVersions: map[string]string{
				"task_analysis": "prompt-v1",
			},
		},
		StartedAt: time.Date(2026, time.November, 25, 10, 0, 0, 0, time.FixedZone("CST", 8*60*60)),
	}

	if err := store.CreateRun(context.Background(), record); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	saved, ok := store.runs["run-1"]
	if !ok {
		t.Fatal("CreateRun() did not save the Run")
	}
	if saved.StartedAt.Location() != time.UTC {
		t.Fatalf("saved start time location = %v, want UTC", saved.StartedAt.Location())
	}
	if saved.Config.PromptVersions["task_analysis"] != "prompt-v1" {
		t.Fatalf("saved prompt references = %+v", saved.Config.PromptVersions)
	}
	if len(store.events["run-1"]) != 0 {
		t.Fatalf("initial event count = %d, want 0", len(store.events["run-1"]))
	}
	if store.nextSequence["run-1"] != 1 {
		t.Fatalf("next sequence = %d, want 1", store.nextSequence["run-1"])
	}
}

func TestMemoryStoreCreateRunRejectsInvalidInput(t *testing.T) {
	store := NewMemoryStore()
	base := runstore.RunRecord{
		RunID:     "run-1",
		RequestID: "request-1",
		TenantID:  "tenant-1",
		Status:    runstore.RunRunning,
		Config: runstore.ConfigReferences{
			ReleaseVersion: "release-v1",
		},
		StartedAt: time.Now(),
	}
	tests := []struct {
		name   string
		record runstore.RunRecord
	}{
		{name: "missing run id", record: func() runstore.RunRecord {
			record := base
			record.RunID = ""
			return record
		}()},
		{name: "missing request id", record: func() runstore.RunRecord {
			record := base
			record.RequestID = ""
			return record
		}()},
		{name: "missing tenant id", record: func() runstore.RunRecord {
			record := base
			record.TenantID = ""
			return record
		}()},
		{name: "not running", record: func() runstore.RunRecord {
			record := base
			record.Status = runstore.RunSucceeded
			return record
		}()},
		{name: "missing release version", record: func() runstore.RunRecord {
			record := base
			record.Config.ReleaseVersion = ""
			return record
		}()},
		{name: "missing start time", record: func() runstore.RunRecord {
			record := base
			record.StartedAt = time.Time{}
			return record
		}()},
		{name: "finished before create", record: func() runstore.RunRecord {
			record := base
			record.FinishedAt = time.Now()
			return record
		}()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := store.CreateRun(context.Background(), test.record); err != runstore.ErrInvalidRun {
				t.Fatalf("CreateRun() error = %v, want %v", err, runstore.ErrInvalidRun)
			}
		})
	}
	if len(store.runs) != 0 {
		t.Fatalf("invalid CreateRun() changed store: %+v", store.runs)
	}
}

func TestMemoryStoreCreateRunRejectsDuplicateAndCanceledContext(t *testing.T) {
	store := NewMemoryStore()
	record := runstore.RunRecord{
		RunID:     "run-1",
		RequestID: "request-1",
		TenantID:  "tenant-1",
		Status:    runstore.RunRunning,
		Config: runstore.ConfigReferences{
			ReleaseVersion: "release-v1",
		},
		StartedAt: time.Now(),
	}
	if err := store.CreateRun(context.Background(), record); err != nil {
		t.Fatalf("first CreateRun() error = %v", err)
	}
	if err := store.CreateRun(context.Background(), record); err != runstore.ErrRunAlreadyExists {
		t.Fatalf("duplicate CreateRun() error = %v, want %v", err, runstore.ErrRunAlreadyExists)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := record
	canceled.RunID = "run-canceled"
	if err := store.CreateRun(ctx, canceled); err != context.Canceled {
		t.Fatalf("canceled CreateRun() error = %v, want %v", err, context.Canceled)
	}
	if _, exists := store.runs["run-canceled"]; exists {
		t.Fatal("canceled CreateRun() changed store")
	}
}

func TestMemoryStoreAppendEventAssignsSequence(t *testing.T) {
	store := NewMemoryStore()
	record := runstore.RunRecord{
		RunID:     "run-1",
		RequestID: "request-1",
		TenantID:  "tenant-1",
		Status:    runstore.RunRunning,
		Config: runstore.ConfigReferences{
			ReleaseVersion: "release-v1",
		},
		StartedAt: time.Now(),
	}
	if err := store.CreateRun(context.Background(), record); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}

	event := runstore.RunEvent{
		Sequence:   99,
		RunID:      "run-1",
		RequestID:  "request-1",
		TenantID:   "tenant-1",
		Type:       runstore.EventModelCall,
		OccurredAt: time.Now(),
		Status:     "success",
	}
	if err := store.AppendEvent(context.Background(), event); err != nil {
		t.Fatalf("AppendEvent() error = %v", err)
	}
	if len(store.events["run-1"]) != 1 {
		t.Fatalf("event count = %d, want 1", len(store.events["run-1"]))
	}
	saved := store.events["run-1"][0]
	if saved.Sequence != 1 {
		t.Fatalf("saved sequence = %d, want 1", saved.Sequence)
	}
	if store.nextSequence["run-1"] != 2 {
		t.Fatalf("next sequence = %d, want 2", store.nextSequence["run-1"])
	}
}

func TestMemoryStoreAppendEventRejectsInvalidBoundary(t *testing.T) {
	store := NewMemoryStore()
	record := runstore.RunRecord{
		RunID:     "run-1",
		RequestID: "request-1",
		TenantID:  "tenant-1",
		Status:    runstore.RunRunning,
		Config: runstore.ConfigReferences{
			ReleaseVersion: "release-v1",
		},
		StartedAt: time.Now(),
	}
	if err := store.CreateRun(context.Background(), record); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}

	base := runstore.RunEvent{
		RunID:      "run-1",
		RequestID:  "request-1",
		TenantID:   "tenant-1",
		Type:       runstore.EventToolCall,
		OccurredAt: time.Now(),
	}
	tests := []struct {
		name  string
		event runstore.RunEvent
		want  error
	}{
		{
			name: "missing run id",
			event: func() runstore.RunEvent {
				event := base
				event.RunID = ""
				return event
			}(),
			want: runstore.ErrInvalidEvent,
		},
		{
			name: "missing occurred at",
			event: func() runstore.RunEvent {
				event := base
				event.OccurredAt = time.Time{}
				return event
			}(),
			want: runstore.ErrInvalidEvent,
		},
		{
			name: "negative duration",
			event: func() runstore.RunEvent {
				event := base
				event.DurationMS = -1
				return event
			}(),
			want: runstore.ErrInvalidEvent,
		},
		{
			name: "unknown event type",
			event: func() runstore.RunEvent {
				event := base
				event.Type = "run.unknown"
				return event
			}(),
			want: runstore.ErrInvalidEventType,
		},
		{
			name: "missing run",
			event: func() runstore.RunEvent {
				event := base
				event.RunID = "missing"
				return event
			}(),
			want: runstore.ErrRunNotFound,
		},
		{
			name: "request mismatch",
			event: func() runstore.RunEvent {
				event := base
				event.RequestID = "other-request"
				return event
			}(),
			want: runstore.ErrRunIdentityMismatch,
		},
		{
			name: "tenant mismatch",
			event: func() runstore.RunEvent {
				event := base
				event.TenantID = "other-tenant"
				return event
			}(),
			want: runstore.ErrRunIdentityMismatch,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := store.AppendEvent(context.Background(), test.event); err != test.want {
				t.Fatalf("AppendEvent() error = %v, want %v", err, test.want)
			}
		})
	}
	if len(store.events["run-1"]) != 0 {
		t.Fatalf("invalid AppendEvent() changed events: %+v", store.events["run-1"])
	}
}

func TestMemoryStoreAppendEventRejectsFinishedRunAndCanceledContext(t *testing.T) {
	store := NewMemoryStore()
	record := runstore.RunRecord{
		RunID:     "run-1",
		RequestID: "request-1",
		TenantID:  "tenant-1",
		Status:    runstore.RunSucceeded,
		Config: runstore.ConfigReferences{
			ReleaseVersion: "release-v1",
		},
		StartedAt:  time.Now().Add(-time.Second),
		FinishedAt: time.Now(),
	}
	store.runs[record.RunID] = record
	store.events[record.RunID] = nil
	store.nextSequence[record.RunID] = 1

	event := runstore.RunEvent{
		RunID:      "run-1",
		RequestID:  "request-1",
		TenantID:   "tenant-1",
		Type:       runstore.EventRunSucceeded,
		OccurredAt: time.Now(),
	}
	if err := store.AppendEvent(context.Background(), event); err != runstore.ErrRunAlreadyFinished {
		t.Fatalf("finished Run error = %v, want %v", err, runstore.ErrRunAlreadyFinished)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	event.RunID = "missing"
	if err := store.AppendEvent(ctx, event); err != context.Canceled {
		t.Fatalf("canceled AppendEvent() error = %v, want %v", err, context.Canceled)
	}
}

func TestMemoryStoreFinishRun(t *testing.T) {
	store := NewMemoryStore()
	record := runstore.RunRecord{
		RunID:     "run-1",
		RequestID: "request-1",
		TenantID:  "tenant-1",
		Status:    runstore.RunRunning,
		Config: runstore.ConfigReferences{
			ReleaseVersion: "release-v1",
		},
		StartedAt: time.Now().Add(-time.Second),
	}
	if err := store.CreateRun(context.Background(), record); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}

	observation := runstore.RunObservation{
		Provider:        "deepseek",
		Model:           "deepseek-chat",
		Duration:        250 * time.Millisecond,
		Success:         true,
		StructuredValid: true,
		TotalTokens:     100,
	}
	if err := store.FinishRun(context.Background(), "run-1", runstore.RunSucceeded, observation); err != nil {
		t.Fatalf("FinishRun() error = %v", err)
	}

	saved := store.runs["run-1"]
	if saved.Status != runstore.RunSucceeded {
		t.Fatalf("status = %q, want %q", saved.Status, runstore.RunSucceeded)
	}
	if saved.FinishedAt.IsZero() {
		t.Fatal("FinishRun() did not set FinishedAt")
	}
	if saved.FinishedAt.Location() != time.UTC {
		t.Fatalf("finished time location = %v, want UTC", saved.FinishedAt.Location())
	}
	if saved.Observation != observation {
		t.Fatalf("observation = %+v, want %+v", saved.Observation, observation)
	}
}

func TestMemoryStoreFinishRunRejectsInvalidTransitions(t *testing.T) {
	store := NewMemoryStore()
	record := runstore.RunRecord{
		RunID:     "run-1",
		RequestID: "request-1",
		TenantID:  "tenant-1",
		Status:    runstore.RunRunning,
		Config: runstore.ConfigReferences{
			ReleaseVersion: "release-v1",
		},
		StartedAt: time.Now(),
	}
	if err := store.CreateRun(context.Background(), record); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}

	if err := store.FinishRun(context.Background(), "run-1", runstore.RunRunning, runstore.RunObservation{}); err != runstore.ErrInvalidStateTransition {
		t.Fatalf("running target status error = %v, want %v", err, runstore.ErrInvalidStateTransition)
	}
	if err := store.FinishRun(context.Background(), "missing", runstore.RunFailed, runstore.RunObservation{}); err != runstore.ErrRunNotFound {
		t.Fatalf("missing Run error = %v, want %v", err, runstore.ErrRunNotFound)
	}
	if err := store.FinishRun(context.Background(), "run-1", runstore.RunFailed, runstore.RunObservation{}); err != nil {
		t.Fatalf("first FinishRun() error = %v", err)
	}
	if err := store.FinishRun(context.Background(), "run-1", runstore.RunSucceeded, runstore.RunObservation{}); err != runstore.ErrRunAlreadyFinished {
		t.Fatalf("duplicate FinishRun() error = %v, want %v", err, runstore.ErrRunAlreadyFinished)
	}
}

func TestMemoryStoreFinishRunStopsWhenContextIsCanceled(t *testing.T) {
	store := NewMemoryStore()
	record := runstore.RunRecord{
		RunID:     "run-1",
		RequestID: "request-1",
		TenantID:  "tenant-1",
		Status:    runstore.RunRunning,
		Config: runstore.ConfigReferences{
			ReleaseVersion: "release-v1",
		},
		StartedAt: time.Now(),
	}
	if err := store.CreateRun(context.Background(), record); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.FinishRun(ctx, "run-1", runstore.RunCanceled, runstore.RunObservation{}); err != context.Canceled {
		t.Fatalf("canceled FinishRun() error = %v, want %v", err, context.Canceled)
	}
	if store.runs["run-1"].Status != runstore.RunRunning {
		t.Fatalf("canceled FinishRun() changed status to %q", store.runs["run-1"].Status)
	}
}

func TestMemoryStoreGetRunReturnsIndependentCopy(t *testing.T) {
	store := NewMemoryStore()
	record := runstore.RunRecord{
		RunID:     "run-1",
		RequestID: "request-1",
		TenantID:  "tenant-1",
		Status:    runstore.RunRunning,
		Config: runstore.ConfigReferences{
			ReleaseVersion: "release-v1",
			PromptVersions: map[string]string{
				"task_analysis": "prompt-v1",
			},
		},
		StartedAt: time.Now(),
	}
	if err := store.CreateRun(context.Background(), record); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}

	got, err := store.GetRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	got.Config.PromptVersions["task_analysis"] = "prompt-mutated"
	got.Config.PromptVersions["new"] = "prompt-new"

	saved := store.runs["run-1"]
	if saved.Config.PromptVersions["task_analysis"] != "prompt-v1" {
		t.Fatalf("stored prompt version was mutated: %+v", saved.Config.PromptVersions)
	}
	if _, exists := saved.Config.PromptVersions["new"]; exists {
		t.Fatalf("stored prompt references gained external key: %+v", saved.Config.PromptVersions)
	}
}

func TestMemoryStoreGetRunRejectsMissingRunAndCanceledContext(t *testing.T) {
	store := NewMemoryStore()
	if _, err := store.GetRun(context.Background(), "missing"); err != runstore.ErrRunNotFound {
		t.Fatalf("missing GetRun() error = %v, want %v", err, runstore.ErrRunNotFound)
	}
	if _, err := store.GetRun(context.Background(), ""); err != runstore.ErrInvalidRun {
		t.Fatalf("empty Run ID error = %v, want %v", err, runstore.ErrInvalidRun)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.GetRun(ctx, "run-1"); err != context.Canceled {
		t.Fatalf("canceled GetRun() error = %v, want %v", err, context.Canceled)
	}
}

func TestMemoryStoreListEventsPreservesOrderAndReturnsCopy(t *testing.T) {
	store := NewMemoryStore()
	record := runstore.RunRecord{
		RunID:     "run-1",
		RequestID: "request-1",
		TenantID:  "tenant-1",
		Status:    runstore.RunRunning,
		Config: runstore.ConfigReferences{
			ReleaseVersion: "release-v1",
		},
		StartedAt: time.Now(),
	}
	if err := store.CreateRun(context.Background(), record); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}
	for _, event := range []runstore.RunEvent{
		{
			RunID:      "run-1",
			RequestID:  "request-1",
			TenantID:   "tenant-1",
			Type:       runstore.EventRunStarted,
			OccurredAt: time.Now(),
		},
		{
			RunID:      "run-1",
			RequestID:  "request-1",
			TenantID:   "tenant-1",
			Type:       runstore.EventModelCall,
			OccurredAt: time.Now(),
		},
	} {
		if err := store.AppendEvent(context.Background(), event); err != nil {
			t.Fatalf("AppendEvent() error = %v", err)
		}
	}

	events, err := store.ListEvents(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("event count = %d, want 2", len(events))
	}
	if events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatalf("event sequences = %d, %d, want 1, 2", events[0].Sequence, events[1].Sequence)
	}
	if events[0].Type != runstore.EventRunStarted || events[1].Type != runstore.EventModelCall {
		t.Fatalf("event order = %+v", events)
	}

	events[0].Type = runstore.EventRunFailed
	if store.events["run-1"][0].Type != runstore.EventRunStarted {
		t.Fatalf("stored event was mutated: %+v", store.events["run-1"])
	}
}

func TestMemoryStoreListEventsRejectsMissingRunAndCanceledContext(t *testing.T) {
	store := NewMemoryStore()
	if _, err := store.ListEvents(context.Background(), "missing"); err != runstore.ErrRunNotFound {
		t.Fatalf("missing ListEvents() error = %v, want %v", err, runstore.ErrRunNotFound)
	}
	if _, err := store.ListEvents(context.Background(), ""); err != runstore.ErrInvalidRun {
		t.Fatalf("empty Run ID error = %v, want %v", err, runstore.ErrInvalidRun)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.ListEvents(ctx, "run-1"); err != context.Canceled {
		t.Fatalf("canceled ListEvents() error = %v, want %v", err, context.Canceled)
	}
}

func TestMemoryStoreSupportsConcurrentCreateAndQuery(t *testing.T) {
	store := NewMemoryStore()
	const runCount = 32

	var wg sync.WaitGroup
	errorsCh := make(chan error, runCount)
	for index := 0; index < runCount; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			runID := "run-" + strconv.Itoa(index)
			record := runstore.RunRecord{
				RunID:     runID,
				RequestID: "request-" + strconv.Itoa(index),
				TenantID:  "tenant-" + strconv.Itoa(index),
				Status:    runstore.RunRunning,
				Config: runstore.ConfigReferences{
					ReleaseVersion: "release-v1",
				},
				StartedAt: time.Now(),
			}
			if err := store.CreateRun(context.Background(), record); err != nil {
				errorsCh <- err
				return
			}
			if _, err := store.GetRun(context.Background(), runID); err != nil {
				errorsCh <- err
				return
			}
			if _, err := store.ListEvents(context.Background(), runID); err != nil {
				errorsCh <- err
			}
		}(index)
	}
	wg.Wait()
	close(errorsCh)

	for err := range errorsCh {
		t.Fatalf("concurrent store operation error = %v", err)
	}
	if len(store.runs) != runCount {
		t.Fatalf("stored Run count = %d, want %d", len(store.runs), runCount)
	}
}

func TestMemoryStoreSupportsConcurrentEventAppendAndFinish(t *testing.T) {
	store := NewMemoryStore()
	record := runstore.RunRecord{
		RunID:     "run-1",
		RequestID: "request-1",
		TenantID:  "tenant-1",
		Status:    runstore.RunRunning,
		Config: runstore.ConfigReferences{
			ReleaseVersion: "release-v1",
		},
		StartedAt: time.Now(),
	}
	if err := store.CreateRun(context.Background(), record); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}

	const eventCount = 64
	var wg sync.WaitGroup
	errorsCh := make(chan error, eventCount)
	for index := 0; index < eventCount; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			err := store.AppendEvent(context.Background(), runstore.RunEvent{
				RunID:      "run-1",
				RequestID:  "request-1",
				TenantID:   "tenant-1",
				Type:       runstore.EventModelCall,
				OccurredAt: time.Now(),
				Status:     "success",
				DurationMS: int64(index),
			})
			if err != nil {
				errorsCh <- err
			}
		}(index)
	}
	wg.Wait()
	close(errorsCh)

	for err := range errorsCh {
		t.Fatalf("concurrent AppendEvent() error = %v", err)
	}

	events, err := store.ListEvents(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	if len(events) != eventCount {
		t.Fatalf("event count = %d, want %d", len(events), eventCount)
	}
	for index, event := range events {
		wantSequence := uint64(index + 1)
		if event.Sequence != wantSequence {
			t.Fatalf("event[%d] sequence = %d, want %d", index, event.Sequence, wantSequence)
		}
	}

	if err := store.FinishRun(context.Background(), "run-1", runstore.RunSucceeded, runstore.RunObservation{
		Success: true,
	}); err != nil {
		t.Fatalf("FinishRun() error = %v", err)
	}
	if _, err := store.GetRun(context.Background(), "run-1"); err != nil {
		t.Fatalf("GetRun() after FinishRun() error = %v", err)
	}
}
