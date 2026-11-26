package memory

import (
	"context"
	"testing"
	"time"

	"agent-project/internal/runstore"
)

// TestMemoryStoreBudgetExceededTerminal 验证超预算事件、终态收口和终态不可变。
func TestMemoryStoreBudgetExceededTerminal(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	record := runstore.RunRecord{
		RunID:     "run-budget",
		RequestID: "request-budget",
		TenantID:  "tenant-1",
		Status:    runstore.RunRunning,
		Config: runstore.ConfigReferences{
			ReleaseVersion: "release-v1",
		},
		StartedAt: time.Now().Add(-time.Second),
	}
	if err := store.CreateRun(ctx, record); err != nil {
		t.Fatalf("CreateRun() error = %v", err)
	}

	budgetEvent := runstore.RunEvent{
		RunID:      record.RunID,
		RequestID:  record.RequestID,
		TenantID:   record.TenantID,
		Type:       runstore.EventRunBudgetExceeded,
		OccurredAt: time.Now(),
		Status:     string(runstore.RunBudgetExceeded),
		ErrorKind:  "token_budget_exceeded",
	}
	if err := store.AppendEvent(ctx, budgetEvent); err != nil {
		t.Fatalf("AppendEvent(budget exceeded) error = %v", err)
	}

	observation := runstore.RunObservation{
		Success:     false,
		TotalTokens: 101,
		ErrorKind:   "token_budget_exceeded",
	}
	if err := store.FinishRun(ctx, record.RunID, runstore.RunBudgetExceeded, observation); err != nil {
		t.Fatalf("FinishRun(budget exceeded) error = %v", err)
	}

	saved, err := store.GetRun(ctx, record.RunID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if saved.Status != runstore.RunBudgetExceeded {
		t.Fatalf("status = %q, want %q", saved.Status, runstore.RunBudgetExceeded)
	}
	if saved.FinishedAt.IsZero() || saved.FinishedAt.Location() != time.UTC {
		t.Fatalf("finished time = %v, want non-zero UTC time", saved.FinishedAt)
	}
	if saved.Observation != observation {
		t.Fatalf("observation = %+v, want %+v", saved.Observation, observation)
	}

	events, err := store.ListEvents(ctx, record.RunID)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("event count = %d, want 1", len(events))
	}
	if events[0].Sequence != 1 || events[0].Type != runstore.EventRunBudgetExceeded ||
		events[0].Status != string(runstore.RunBudgetExceeded) {
		t.Fatalf("budget event = %+v, want sequenced terminal event", events[0])
	}

	if err := store.FinishRun(ctx, record.RunID, runstore.RunSucceeded, runstore.RunObservation{}); err != runstore.ErrRunAlreadyFinished {
		t.Fatalf("duplicate FinishRun() error = %v, want %v", err, runstore.ErrRunAlreadyFinished)
	}
	afterTerminal := runstore.RunEvent{
		RunID:      record.RunID,
		RequestID:  record.RequestID,
		TenantID:   record.TenantID,
		Type:       runstore.EventModelCall,
		OccurredAt: time.Now(),
	}
	if err := store.AppendEvent(ctx, afterTerminal); err != runstore.ErrRunAlreadyFinished {
		t.Fatalf("AppendEvent(after terminal) error = %v, want %v", err, runstore.ErrRunAlreadyFinished)
	}

	// 重复完成和终态后追加事件都不能改变超预算状态或事件历史。
	saved, err = store.GetRun(ctx, record.RunID)
	if err != nil {
		t.Fatalf("GetRun(after rejected writes) error = %v", err)
	}
	if saved.Status != runstore.RunBudgetExceeded || saved.Observation != observation {
		t.Fatalf("terminal Run changed after rejected writes: %+v", saved)
	}
	events, err = store.ListEvents(ctx, record.RunID)
	if err != nil {
		t.Fatalf("ListEvents(after rejected writes) error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("event count after rejected writes = %d, want 1", len(events))
	}
}
