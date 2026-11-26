package agent

import (
	"errors"
	"testing"
	"time"
)

// requireBudgetViolation 验证错误分类和完整结构化违规字段。
func requireBudgetViolation(t *testing.T, err error, want BudgetViolation) {
	t.Helper()
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("error = %v, want ErrBudgetExceeded", err)
	}
	var got BudgetViolation
	if !errors.As(err, &got) {
		t.Fatalf("error = %v, want BudgetViolation", err)
	}
	if got != want {
		t.Fatalf("BudgetViolation = %+v, want %+v", got, want)
	}
}

// TestNewBudgetController 验证控制器初始化、开始时间保留和非法配置拒绝。
func TestNewBudgetController(t *testing.T) {
	started := time.Now()
	controller, err := newBudgetController(BudgetSet{
		Run: BudgetLimits{MaxSteps: 5},
	}, started)
	if err != nil {
		t.Fatalf("newBudgetController() error = %v", err)
	}
	if controller == nil {
		t.Fatal("newBudgetController() returned nil controller")
	}
	if controller.started != started {
		t.Fatalf("controller.started = %v, want original value %v", controller.started, started)
	}
	if controller.effective.Limits.MaxSteps != 5 || controller.effective.StepsSource != BudgetScopeRun {
		t.Fatalf("effective budget = %+v, want five steps from run", controller.effective)
	}

	controller, err = newBudgetController(BudgetSet{
		User: BudgetLimits{MaxTotalTokens: -1},
	}, started)
	if !errors.Is(err, ErrInvalidBudget) || controller != nil {
		t.Fatalf("newBudgetController() = (%v, %v), want (nil, ErrInvalidBudget)", controller, err)
	}
}

// TestBudgetControllerTimeBudget 验证剩余时间、时钟倒退和总时间超限。
func TestBudgetControllerTimeBudget(t *testing.T) {
	started := time.Date(2026, time.November, 26, 10, 0, 0, 0, time.UTC)
	controller, err := newBudgetController(BudgetSet{
		Run: BudgetLimits{MaxDuration: time.Minute},
	}, started)
	if err != nil {
		t.Fatalf("newBudgetController() error = %v", err)
	}

	if got := controller.RemainingDuration(started); got != time.Minute {
		t.Fatalf("RemainingDuration(start) = %v, want %v", got, time.Minute)
	}
	if got := controller.RemainingDuration(started.Add(-time.Second)); got != time.Minute {
		t.Fatalf("RemainingDuration(clock rollback) = %v, want %v", got, time.Minute)
	}
	if got := controller.RemainingDuration(started.Add(30 * time.Second)); got != 30*time.Second {
		t.Fatalf("RemainingDuration(half) = %v, want %v", got, 30*time.Second)
	}
	if err := controller.checkElapsed(started.Add(59 * time.Second)); err != nil {
		t.Fatalf("checkElapsed(before deadline) error = %v", err)
	}

	requireBudgetViolation(t, controller.checkElapsed(started.Add(time.Minute)), BudgetViolation{
		Scope:  BudgetScopeRun,
		Reason: "total_time_budget_exceeded",
		Limit:  60000,
		Used:   60000,
	})
	if got := controller.RemainingDuration(started.Add(2 * time.Minute)); got != 0 {
		t.Fatalf("RemainingDuration(after deadline) = %v, want zero", got)
	}

	var nilController *BudgetController
	if got := nilController.RemainingDuration(started); got != 0 {
		t.Fatalf("nil RemainingDuration() = %v, want zero", got)
	}
	if err := nilController.checkElapsed(started); !errors.Is(err, ErrInvalidBudget) {
		t.Fatalf("nil checkElapsed() error = %v, want ErrInvalidBudget", err)
	}
}

// TestBudgetControllerStepBudget 验证恰好达到步数上限和拒绝后不计步。
func TestBudgetControllerStepBudget(t *testing.T) {
	started := time.Date(2026, time.November, 26, 10, 0, 0, 0, time.UTC)
	controller, err := newBudgetController(BudgetSet{
		Run: BudgetLimits{MaxSteps: 2, MaxDuration: time.Hour},
	}, started)
	if err != nil {
		t.Fatalf("newBudgetController() error = %v", err)
	}

	if err := controller.BeginStep(started); err != nil {
		t.Fatalf("BeginStep(first) error = %v", err)
	}
	if err := controller.BeginStep(started.Add(time.Second)); err != nil {
		t.Fatalf("BeginStep(second) error = %v", err)
	}
	if controller.steps != 2 {
		t.Fatalf("controller.steps = %d, want 2", controller.steps)
	}

	requireBudgetViolation(t, controller.BeginStep(started.Add(2*time.Second)), BudgetViolation{
		Scope:  BudgetScopeRun,
		Reason: "max_steps_exceeded",
		Limit:  2,
		Used:   2,
	})
	if controller.steps != 2 {
		t.Fatalf("steps after rejected operation = %d, want 2", controller.steps)
	}

	var nilController *BudgetController
	if err := nilController.BeginStep(started); !errors.Is(err, ErrInvalidBudget) {
		t.Fatalf("nil BeginStep() error = %v, want ErrInvalidBudget", err)
	}
}

// TestBudgetControllerToolBudget 验证工具专项预算和通用步数预算的结算顺序。
func TestBudgetControllerToolBudget(t *testing.T) {
	started := time.Date(2026, time.November, 26, 10, 0, 0, 0, time.UTC)
	controller, err := newBudgetController(BudgetSet{
		Run: BudgetLimits{MaxSteps: 2, MaxToolCalls: 1, MaxDuration: time.Hour},
	}, started)
	if err != nil {
		t.Fatalf("newBudgetController() error = %v", err)
	}
	if err := controller.BeginToolCall(started); err != nil {
		t.Fatalf("BeginToolCall(first) error = %v", err)
	}
	if controller.steps != 1 || controller.toolCalls != 1 {
		t.Fatalf("usage = (%d steps, %d tools), want (1, 1)", controller.steps, controller.toolCalls)
	}

	requireBudgetViolation(t, controller.BeginToolCall(started.Add(time.Second)), BudgetViolation{
		Scope:  BudgetScopeRun,
		Reason: "max_tool_calls_exceeded",
		Limit:  1,
		Used:   1,
	})
	if controller.steps != 1 || controller.toolCalls != 1 {
		t.Fatalf("rejected tool changed usage = (%d, %d), want (1, 1)", controller.steps, controller.toolCalls)
	}

	stepLimited, err := newBudgetController(BudgetSet{
		Run: BudgetLimits{MaxSteps: 1, MaxToolCalls: 3, MaxDuration: time.Hour},
	}, started)
	if err != nil {
		t.Fatalf("newBudgetController(step limited) error = %v", err)
	}
	if err := stepLimited.BeginToolCall(started); err != nil {
		t.Fatalf("BeginToolCall(step limited first) error = %v", err)
	}
	requireBudgetViolation(t, stepLimited.BeginToolCall(started.Add(time.Second)), BudgetViolation{
		Scope:  BudgetScopeRun,
		Reason: "max_steps_exceeded",
		Limit:  1,
		Used:   1,
	})
	if stepLimited.toolCalls != 1 {
		t.Fatalf("tool calls after step rejection = %d, want 1", stepLimited.toolCalls)
	}

	var nilController *BudgetController
	if err := nilController.BeginToolCall(started); !errors.Is(err, ErrInvalidBudget) {
		t.Fatalf("nil BeginToolCall() error = %v, want ErrInvalidBudget", err)
	}
}

// TestBudgetControllerTokenBudget 验证模型前检查、恰好达到、超出和非法消耗。
func TestBudgetControllerTokenBudget(t *testing.T) {
	started := time.Date(2026, time.November, 26, 10, 0, 0, 0, time.UTC)
	limits := BudgetSet{Run: BudgetLimits{
		MaxSteps:       10,
		MaxTotalTokens: 100,
		MaxDuration:    time.Hour,
	}}
	controller, err := newBudgetController(limits, started)
	if err != nil {
		t.Fatalf("newBudgetController() error = %v", err)
	}

	if err := controller.BeginModelStep(started); err != nil {
		t.Fatalf("BeginModelStep(first) error = %v", err)
	}
	if err := controller.ChargeTokens(80); err != nil {
		t.Fatalf("ChargeTokens(80) error = %v", err)
	}
	if err := controller.BeginModelStep(started.Add(time.Second)); err != nil {
		t.Fatalf("BeginModelStep(second) error = %v", err)
	}
	if err := controller.ChargeTokens(20); err != nil {
		t.Fatalf("ChargeTokens(exact limit) error = %v", err)
	}
	if controller.totalTokens != 100 {
		t.Fatalf("controller.totalTokens = %d, want 100", controller.totalTokens)
	}

	requireBudgetViolation(t, controller.BeginModelStep(started.Add(2*time.Second)), BudgetViolation{
		Scope:  BudgetScopeRun,
		Reason: "token_budget_exceeded",
		Limit:  100,
		Used:   100,
	})
	if controller.steps != 2 {
		t.Fatalf("steps after token rejection = %d, want 2", controller.steps)
	}

	overLimit, err := newBudgetController(limits, started)
	if err != nil {
		t.Fatalf("newBudgetController(over limit) error = %v", err)
	}
	if err := overLimit.BeginModelStep(started); err != nil {
		t.Fatalf("BeginModelStep(over limit) error = %v", err)
	}
	requireBudgetViolation(t, overLimit.ChargeTokens(101), BudgetViolation{
		Scope:  BudgetScopeRun,
		Reason: "token_budget_exceeded",
		Limit:  100,
		Used:   101,
	})
	if overLimit.totalTokens != 101 {
		t.Fatalf("over-limit totalTokens = %d, want actual usage 101", overLimit.totalTokens)
	}
	if err := overLimit.ChargeTokens(-1); !errors.Is(err, ErrInvalidBudget) {
		t.Fatalf("ChargeTokens(negative) error = %v, want ErrInvalidBudget", err)
	}
	if overLimit.totalTokens != 101 {
		t.Fatalf("negative charge changed totalTokens = %d, want 101", overLimit.totalTokens)
	}

	var nilController *BudgetController
	if err := nilController.BeginModelStep(started); !errors.Is(err, ErrInvalidBudget) {
		t.Fatalf("nil BeginModelStep() error = %v, want ErrInvalidBudget", err)
	}
	if err := nilController.ChargeTokens(1); !errors.Is(err, ErrInvalidBudget) {
		t.Fatalf("nil ChargeTokens() error = %v, want ErrInvalidBudget", err)
	}
}

// TestBudgetControllerEstimatedTokenBudget 验证估算消耗、来源数量和超限时账本保留。
func TestBudgetControllerEstimatedTokenBudget(t *testing.T) {
	started := time.Date(2026, time.November, 26, 10, 0, 0, 0, time.UTC)
	controller, err := newBudgetController(BudgetSet{Run: BudgetLimits{
		MaxTotalTokens: 5,
		MaxDuration:    time.Hour,
	}}, started)
	if err != nil {
		t.Fatalf("newBudgetController() error = %v", err)
	}

	if err := controller.ChargeTokens(2); err != nil {
		t.Fatalf("ChargeTokens(real usage) error = %v", err)
	}
	if err := controller.ChargeEstimatedTokens(3); err != nil {
		t.Fatalf("ChargeEstimatedTokens(exact limit) error = %v", err)
	}
	if controller.totalTokens != 5 || controller.estimatedTokens != 3 {
		t.Fatalf("token ledger = (%d, %d), want total 5 and estimated 3", controller.totalTokens, controller.estimatedTokens)
	}

	requireBudgetViolation(t, controller.ChargeEstimatedTokens(2), BudgetViolation{
		Scope:  BudgetScopeRun,
		Reason: "token_budget_exceeded",
		Limit:  5,
		Used:   7,
	})
	if controller.totalTokens != 7 || controller.estimatedTokens != 5 {
		t.Fatalf("over-limit ledger = (%d, %d), want total 7 and estimated 5", controller.totalTokens, controller.estimatedTokens)
	}
	if err := controller.ChargeEstimatedTokens(-1); !errors.Is(err, ErrInvalidBudget) {
		t.Fatalf("ChargeEstimatedTokens(negative) error = %v, want ErrInvalidBudget", err)
	}
	if controller.totalTokens != 7 || controller.estimatedTokens != 5 {
		t.Fatalf("negative charge changed ledger = (%d, %d)", controller.totalTokens, controller.estimatedTokens)
	}

	var nilController *BudgetController
	if err := nilController.ChargeEstimatedTokens(1); !errors.Is(err, ErrInvalidBudget) {
		t.Fatalf("nil ChargeEstimatedTokens() error = %v, want ErrInvalidBudget", err)
	}
}

// TestBudgetControllerRemainingTokens 验证剩余额度、恰好达限、超限和空控制器。
func TestBudgetControllerRemainingTokens(t *testing.T) {
	controller, err := newBudgetController(BudgetSet{Run: BudgetLimits{
		MaxTotalTokens: 10,
		MaxDuration:    time.Hour,
	}}, time.Now())
	if err != nil {
		t.Fatalf("newBudgetController() error = %v", err)
	}
	if got := controller.RemainingTokens(); got != 10 {
		t.Fatalf("initial RemainingTokens() = %d, want 10", got)
	}
	if err := controller.ChargeTokens(4); err != nil {
		t.Fatalf("ChargeTokens() error = %v", err)
	}
	if err := controller.ChargeEstimatedTokens(2); err != nil {
		t.Fatalf("ChargeEstimatedTokens() error = %v", err)
	}
	if got := controller.RemainingTokens(); got != 4 {
		t.Fatalf("RemainingTokens() after mixed charges = %d, want 4", got)
	}
	if err := controller.ChargeTokens(4); err != nil {
		t.Fatalf("ChargeTokens(exact limit) error = %v", err)
	}
	if got := controller.RemainingTokens(); got != 0 {
		t.Fatalf("RemainingTokens() at limit = %d, want 0", got)
	}

	overLimit, err := newBudgetController(BudgetSet{Run: BudgetLimits{
		MaxTotalTokens: 1,
		MaxDuration:    time.Hour,
	}}, time.Now())
	if err != nil {
		t.Fatalf("newBudgetController(over limit) error = %v", err)
	}
	requireBudgetViolation(t, overLimit.ChargeTokens(2), BudgetViolation{
		Scope:  BudgetScopeRun,
		Reason: "token_budget_exceeded",
		Limit:  1,
		Used:   2,
	})
	if got := overLimit.RemainingTokens(); got != 0 {
		t.Fatalf("RemainingTokens() over limit = %d, want 0", got)
	}

	var nilController *BudgetController
	if got := nilController.RemainingTokens(); got != 0 {
		t.Fatalf("nil RemainingTokens() = %d, want 0", got)
	}
}
