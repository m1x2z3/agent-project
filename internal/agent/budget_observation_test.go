package agent

import (
	"context"
	"errors"
	"testing"

	"agent-project/internal/runstore"
)

// TestClassifyRuntimeErrorBudget 验证预算原因、通用回退和外部 Context 分类。
func TestClassifyRuntimeErrorBudget(t *testing.T) {
	tests := []struct {
		name string // 描述当前错误分类场景。
		err  error  // 保存待分类的运行错误。
		want string // 保存期望的稳定观测分类。
	}{
		{
			name: "max steps",
			err:  BudgetViolation{Scope: BudgetScopeRun, Reason: "max_steps_exceeded"},
			want: "max_steps_exceeded",
		},
		{
			name: "wrapped token budget",
			err: errors.Join(BudgetViolation{
				Scope:  BudgetScopeTenant,
				Reason: "token_budget_exceeded",
			}),
			want: "token_budget_exceeded",
		},
		{
			name: "total time budget",
			err:  BudgetViolation{Scope: BudgetScopeDefault, Reason: "total_time_budget_exceeded"},
			want: "total_time_budget_exceeded",
		},
		{name: "generic budget error", err: ErrBudgetExceeded, want: "budget_exceeded"},
		{name: "no progress loop", err: ErrLoopDetected, want: "loop_detected"},
		{name: "caller cancellation", err: context.Canceled, want: "canceled"},
		{name: "caller deadline", err: context.DeadlineExceeded, want: "timeout"},
		{name: "ordinary error", err: errors.New("ordinary"), want: "runtime_error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyRuntimeError(tt.err); got != tt.want {
				t.Fatalf("classifyRuntimeError() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestClassifyRunCompletionBudget 验证预算、成功、取消和普通失败的终态映射。
func TestClassifyRunCompletionBudget(t *testing.T) {
	tests := []struct {
		name       string             // 描述当前完成映射场景。
		err        error              // 保存当前 Run 的结束错误。
		wantStatus runstore.RunStatus // 保存期望的 Run 终态。
		wantEvent  runstore.EventType // 保存期望的结束事件。
	}{
		{
			name:       "budget exceeded",
			err:        BudgetViolation{Reason: "max_tool_calls_exceeded"},
			wantStatus: runstore.RunBudgetExceeded,
			wantEvent:  runstore.EventRunBudgetExceeded,
		},
		{
			name:       "succeeded",
			err:        nil,
			wantStatus: runstore.RunSucceeded,
			wantEvent:  runstore.EventRunSucceeded,
		},
		{
			name:       "caller canceled",
			err:        context.Canceled,
			wantStatus: runstore.RunCanceled,
			wantEvent:  runstore.EventRunCanceled,
		},
		{
			name:       "caller deadline",
			err:        context.DeadlineExceeded,
			wantStatus: runstore.RunCanceled,
			wantEvent:  runstore.EventRunCanceled,
		},
		{
			name:       "no progress loop",
			err:        ErrLoopDetected,
			wantStatus: runstore.RunFailed,
			wantEvent:  runstore.EventRunFailed,
		},
		{
			name:       "ordinary failure",
			err:        errors.New("ordinary"),
			wantStatus: runstore.RunFailed,
			wantEvent:  runstore.EventRunFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, event := classifyRunCompletion(tt.err)
			if status != tt.wantStatus || event != tt.wantEvent {
				t.Fatalf("classifyRunCompletion() = (%q, %q), want (%q, %q)", status, event, tt.wantStatus, tt.wantEvent)
			}
		})
	}
}
