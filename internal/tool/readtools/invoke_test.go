package readtools

import (
	"context"
	"errors"
	"testing"

	"agent-project/internal/tool/tooling"
)

func TestInvokeWithObservationRecordsBoundaryRejection(t *testing.T) {
	observer := &recordingObserver{}
	tool := NewCurrentTimeTool(nil, nil)
	request := currentTimeRequest(`{"timezone":"UTC"}`)
	request.CapabilityToken = tooling.CapabilityToken{}

	_, err := InvokeWithObservation(context.Background(), tool, request, observer)
	if !errors.Is(err, tooling.ErrCapabilityTokenMissing) {
		t.Fatalf("InvokeWithObservation() error = %v, want missing token", err)
	}
	if len(observer.items) != 1 {
		t.Fatalf("observation count = %d, want 1", len(observer.items))
	}
	observation := observer.items[0]
	if observation.ToolName != GetCurrentTimeName ||
		observation.ResultStatus != tooling.ResultForbidden ||
		observation.ErrorCode != "forbidden" {
		t.Fatalf("unexpected boundary observation: %+v", observation)
	}
}

func TestInvokeWithObservationRecordsTenantBoundaryRejection(t *testing.T) {
	observer := &recordingObserver{}
	tool := NewSearchProjectDocsTool(NewMemoryProjectDocStore(nil), nil)
	request := projectDocsRequest(`{"project_id":"project-a","query":"error","limit":10}`)
	request.TenantID = "tenant-b"

	_, err := InvokeWithObservation(context.Background(), tool, request, observer)
	if !errors.Is(err, tooling.ErrTenantMismatch) {
		t.Fatalf("InvokeWithObservation() error = %v, want tenant mismatch", err)
	}
	if len(observer.items) != 1 {
		t.Fatalf("observation count = %d, want 1", len(observer.items))
	}
	if observer.items[0].ResultStatus != tooling.ResultForbidden {
		t.Fatalf("boundary status = %q, want forbidden", observer.items[0].ResultStatus)
	}
}

func TestInvokeWithObservationDoesNotDuplicateToolObservation(t *testing.T) {
	observer := &recordingObserver{}
	tool := NewCurrentTimeTool(nil, observer)

	_, err := InvokeWithObservation(
		context.Background(),
		tool,
		currentTimeRequest(`{"timezone":"Not/AZone"}`),
		observer,
	)
	if err != nil {
		t.Fatalf("InvokeWithObservation() error = %v", err)
	}
	if len(observer.items) != 1 {
		t.Fatalf("observation count = %d, want 1", len(observer.items))
	}
	if observer.items[0].ResultStatus != tooling.ResultInvalidArgument ||
		observer.items[0].ErrorCode != "invalid_timezone" {
		t.Fatalf("unexpected tool observation: %+v", observer.items[0])
	}
}

func TestInvokeWithObservationRecordsCanceledContext(t *testing.T) {
	observer := &recordingObserver{}
	tool := NewCurrentTimeTool(nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := InvokeWithObservation(
		ctx,
		tool,
		currentTimeRequest(`{"timezone":"UTC"}`),
		observer,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("InvokeWithObservation() error = %v, want context.Canceled", err)
	}
	if len(observer.items) != 1 ||
		observer.items[0].ResultStatus != tooling.ResultInternalError ||
		observer.items[0].ErrorCode != "canceled" {
		t.Fatalf("unexpected cancellation observation: %+v", observer.items)
	}
}
