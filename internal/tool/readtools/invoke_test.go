package readtools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

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

func TestInvokeWithObservationRecordsSuccessfulToolWithOwnObserverDisabled(t *testing.T) {
	observer := &recordingObserver{}
	tool := NewCurrentTimeTool(nil, nil)

	_, err := InvokeWithObservation(
		context.Background(),
		tool,
		currentTimeRequest(`{"timezone":"UTC"}`),
		observer,
	)
	if err != nil {
		t.Fatalf("InvokeWithObservation() error = %v", err)
	}
	if len(observer.items) != 1 ||
		observer.items[0].ResultStatus != tooling.ResultSuccess {
		t.Fatalf("unexpected success observation: %+v", observer.items)
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

func TestInvokeWithObservationRecordsResultValidationFailure(t *testing.T) {
	observer := &recordingObserver{}
	tool := invalidResultTool{}

	_, err := InvokeWithObservation(
		context.Background(),
		&tool,
		currentTimeRequest(`{"timezone":"UTC"}`),
		observer,
	)
	if !errors.Is(err, tooling.ErrResultInvalid) {
		t.Fatalf("InvokeWithObservation() error = %v, want ErrResultInvalid", err)
	}
	if len(observer.items) != 1 {
		t.Fatalf("observation count = %d, want 1", len(observer.items))
	}
	if observer.items[0].ResultStatus != tooling.ResultInternalError ||
		observer.items[0].ErrorCode != "execution_failed" {
		t.Fatalf("unexpected result validation observation: %+v", observer.items[0])
	}
}

type invalidResultTool struct{}

func (invalidResultTool) Definition() tooling.ToolDefinition {
	return tooling.ToolDefinition{
		Name:            GetCurrentTimeName,
		Version:         ReadToolVersion,
		Description:     "测试结果校验失败",
		Permission:      tooling.Permission("time:read"),
		RiskLevel:       tooling.RiskLow,
		EffectClass:     tooling.EffectRead,
		Timeout:         time.Second,
		ParameterSchema: GetCurrentTimeParameterSchema(),
		TenantScope:     tooling.TenantScopeGlobal,
	}
}

func (invalidResultTool) Execute(context.Context, tooling.ToolExecutionRequest) (tooling.ToolResult, error) {
	return tooling.ToolResult{
		ToolName:    GetCurrentTimeName,
		ToolVersion: ReadToolVersion,
		Status:      tooling.ResultSuccess,
		Data:        json.RawMessage(`not-json`),
	}, nil
}
