package readtools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agent-project/internal/tool/tooling"
)

func TestCurrentTimeToolReturnsTimeInRequestedTimezone(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 34, 56, 0, time.UTC)
	observer := &recordingObserver{}
	tool := NewCurrentTimeTool(func() time.Time { return now }, observer)
	request := currentTimeRequest(`{"timezone":"Asia/Shanghai"}`)

	result, err := tooling.Invoke(context.Background(), tool, request)
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if result.Status != tooling.ResultSuccess {
		t.Fatalf("result status = %q, want success", result.Status)
	}

	var payload currentTimeResult
	if err := json.Unmarshal(result.Data, &payload); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if payload.CurrentTime != "2026-11-21T20:34:56+08:00" {
		t.Fatalf("current time = %q", payload.CurrentTime)
	}
	if payload.Timezone != "Asia/Shanghai" || payload.UnixTimestamp != now.Unix() {
		t.Fatalf("unexpected result: %+v", payload)
	}
	if len(observer.items) != 1 || observer.items[0].ResultStatus != tooling.ResultSuccess {
		t.Fatalf("unexpected observations: %+v", observer.items)
	}
	if observer.items[0].ArgumentsDigest == "" ||
		observer.items[0].ArgumentsDigest == string(request.Arguments) {
		t.Fatalf("observation contains raw arguments: %+v", observer.items[0])
	}
}

func TestCurrentTimeToolRejectsInvalidTimezone(t *testing.T) {
	tool := NewCurrentTimeTool(time.Now, nil)
	result, err := tooling.Invoke(
		context.Background(),
		tool,
		currentTimeRequest(`{"timezone":"Mars/Olympus"}`),
	)
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if result.Status != tooling.ResultInvalidArgument {
		t.Fatalf("result status = %q, want invalid_argument", result.Status)
	}
	if result.Error == nil || result.Error.Code != "invalid_timezone" {
		t.Fatalf("unexpected tool error: %+v", result.Error)
	}
}

func TestCurrentTimeToolContextCancellationStopsExecution(t *testing.T) {
	called := false
	tool := NewCurrentTimeTool(func() time.Time {
		called = true
		return time.Now()
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tooling.Invoke(ctx, tool, currentTimeRequest(`{"timezone":"UTC"}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Invoke() error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("clock was called after context cancellation")
	}
}

func TestCurrentTimeToolDefinitionIsStable(t *testing.T) {
	tool := NewCurrentTimeTool(nil, nil)
	definition := tool.Definition()

	if definition.Name != GetCurrentTimeName ||
		definition.Version != ReadToolVersion ||
		definition.EffectClass != tooling.EffectRead ||
		definition.RiskLevel != tooling.RiskLow {
		t.Fatalf("unexpected definition: %+v", definition)
	}
	if err := definition.Validate(); err != nil {
		t.Fatalf("definition rejected: %v", err)
	}
}

type recordingObserver struct {
	items []ToolObservation
}

func (o *recordingObserver) Observe(observation ToolObservation) {
	o.items = append(o.items, observation)
}

func currentTimeRequest(arguments string) tooling.ToolExecutionRequest {
	return tooling.ToolExecutionRequest{
		ToolName:    GetCurrentTimeName,
		ToolVersion: ReadToolVersion,
		TenantID:    "tenant-a",
		RunID:       "run-1",
		RequestID:   "req-1",
		Arguments:   json.RawMessage(arguments),
		CapabilityToken: tooling.CapabilityToken{
			TokenID:     "token-1",
			TenantID:    "tenant-a",
			RunID:       "run-1",
			ToolName:    GetCurrentTimeName,
			ToolVersion: ReadToolVersion,
			Permissions: []tooling.Permission{"time:read"},
			ExpiresAt:   time.Now().Add(time.Hour),
			Nonce:       "nonce-1",
		},
	}
}
