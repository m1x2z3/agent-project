package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agent-project/internal/recording"
	"agent-project/internal/runstore/memory"
	"agent-project/internal/structuredoutput"
	"agent-project/internal/tool/runner"
	"agent-project/internal/tool/tooling"
)

// fixedRecordingClock 为单次测试 Run 提供固定时间，消除真实等待和系统时间波动。
type fixedRecordingClock struct {
	now time.Time // 保存每次 Now 调用返回的确定性时间。
}

// Now 返回注入的固定时间，供预算、观测、RunStore 和工具令牌共同使用。
func (c fixedRecordingClock) Now() time.Time {
	return c.now
}

// TestRuntimeUsesRequestClockForRunStoreAndObservation 验证普通 Runtime 的全链路时间来源。
func TestRuntimeUsesRequestClockForRunStoreAndObservation(t *testing.T) {
	fixed := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.FixedZone("UTC+8", 8*60*60))
	store := memory.NewMemoryStore()
	runtime := newRuntimeWithRunStore(t, &fakeRuntimeModel{}, store)
	input := RunInput{
		RequestID: "request-runtime-fixed-clock",
		TenantID:  "tenant-a",
		RunID:     "run-runtime-fixed-clock",
		Query:     "定位订单服务错误",
	}
	ctx := recording.WithClock(context.Background(), fixedRecordingClock{now: fixed})

	if _, err := runtime.Run(ctx, input); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	record, err := store.GetRun(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if !record.StartedAt.Equal(fixed) || !record.FinishedAt.Equal(fixed) {
		t.Fatalf("Run times = [%v, %v], want injected time %v", record.StartedAt, record.FinishedAt, fixed)
	}
	if record.Observation.Duration != 0 {
		t.Fatalf("Run duration = %v, want 0 with fixed Clock", record.Observation.Duration)
	}
	events, err := store.ListEvents(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("events count = %d, want 4", len(events))
	}
	for _, event := range events {
		if !event.OccurredAt.Equal(fixed) || event.OccurredAt.Location() != time.UTC {
			t.Fatalf("event time = %v, want injected UTC time %v", event.OccurredAt, fixed)
		}
	}
}

// TestToolLoopUsesRequestClockAcrossToolBoundaries 验证工具授权、Executor 和 Invoke 共享请求时钟。
func TestToolLoopUsesRequestClockAcrossToolBoundaries(t *testing.T) {
	fixed := time.Date(2020, time.January, 2, 3, 4, 5, 0, time.UTC)
	model := &toolLoopSequenceModel{decisions: []string{
		`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"clock"}}`,
		`{"should_call":false}`,
	}}
	tool := &toolLoopTestTool{
		definition: toolLoopExecutionDefinition(),
		result: tooling.ToolResult{
			Status: tooling.ResultSuccess,
			Data:   json.RawMessage(`{"hits":[]}`),
		},
	}
	store := memory.NewMemoryStore()
	runtime := newToolLoopRunStoreRuntime(t, model, tool, store, func(
		_ context.Context,
		input RunInput,
		decision structuredoutput.ToolDecision,
	) (runner.ToolCallRequest, error) {
		request := validToolLoopCallRequest(input, decision)
		// 令牌相对固定时间有效、相对真实当前时间早已过期；任一执行边界若绕过
		// 注入 Clock 而读取系统时间，本次 Run 都会在进入工具实现前失败。
		request.CapabilityToken.ExpiresAt = fixed.Add(time.Hour)
		return request, nil
	})
	input := toolLoopRunInput("run-tool-loop-fixed-clock")
	ctx := recording.WithClock(context.Background(), fixedRecordingClock{now: fixed})

	if _, err := runtime.Run(ctx, input); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if tool.executeCalls != 1 {
		t.Fatalf("tool calls = %d, want 1", tool.executeCalls)
	}
	record, err := store.GetRun(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if !record.StartedAt.Equal(fixed) || !record.FinishedAt.Equal(fixed) || record.Observation.Duration != 0 {
		t.Fatalf("stored Run timing = %+v, want fixed Clock and zero duration", record)
	}
	events, err := store.ListEvents(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	for _, event := range events {
		if !event.OccurredAt.Equal(fixed) {
			t.Fatalf("event %q time = %v, want %v", event.Type, event.OccurredAt, fixed)
		}
	}
}
