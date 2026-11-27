package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-project/internal/runstore"
	"agent-project/internal/structuredoutput"
)

// TestValidateBudgetLimits 验证单组预算的零值、合法值和负数边界。
func TestValidateBudgetLimits(t *testing.T) {
	tests := []struct {
		name   string       // 描述当前校验场景。
		limits BudgetLimits // 保存待校验的预算值。
		want   error        // 保存期望返回的稳定错误。
	}{
		{name: "zero values mean unset", limits: BudgetLimits{}, want: nil},
		{
			name: "positive values are valid",
			limits: BudgetLimits{
				MaxSteps:       1,
				MaxToolCalls:   2,
				MaxTotalTokens: 3,
				MaxDuration:    time.Second,
			},
			want: nil,
		},
		{name: "negative steps", limits: BudgetLimits{MaxSteps: -1}, want: ErrInvalidBudget},
		{name: "negative tool calls", limits: BudgetLimits{MaxToolCalls: -1}, want: ErrInvalidBudget},
		{name: "negative tokens", limits: BudgetLimits{MaxTotalTokens: -1}, want: ErrInvalidBudget},
		{name: "negative duration", limits: BudgetLimits{MaxDuration: -time.Second}, want: ErrInvalidBudget},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBudgetLimits(tt.limits)
			if !errors.Is(err, tt.want) {
				t.Fatalf("validateBudgetLimits() error = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestValidateBudgetSet 验证三级预算中的任意非法作用域都会使整个配置被拒绝。
func TestValidateBudgetSet(t *testing.T) {
	tests := []struct {
		name string    // 描述当前三级预算校验场景。
		set  BudgetSet // 保存待校验的三级预算。
		want error     // 保存期望返回的稳定错误。
	}{
		{name: "empty set is valid", set: BudgetSet{}, want: nil},
		{
			name: "all scopes are valid",
			set: BudgetSet{
				User:   BudgetLimits{MaxSteps: 1},
				Tenant: BudgetLimits{MaxToolCalls: 1},
				Run:    BudgetLimits{MaxTotalTokens: 1},
			},
			want: nil,
		},
		{name: "invalid user budget", set: BudgetSet{User: BudgetLimits{MaxSteps: -1}}, want: ErrInvalidBudget},
		{name: "invalid tenant budget", set: BudgetSet{Tenant: BudgetLimits{MaxToolCalls: -1}}, want: ErrInvalidBudget},
		{name: "invalid run budget", set: BudgetSet{Run: BudgetLimits{MaxDuration: -time.Second}}, want: ErrInvalidBudget},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBudgetSet(tt.set)
			if !errors.Is(err, tt.want) {
				t.Fatalf("validateBudgetSet() error = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestDefaultBudgetLimits 验证安全默认值以及每次返回值的修改隔离性。
func TestDefaultBudgetLimits(t *testing.T) {
	want := BudgetLimits{
		MaxSteps:       16,
		MaxToolCalls:   3,
		MaxTotalTokens: 100000,
		MaxDuration:    2 * time.Minute,
	}
	if got := defaultBudgetLimits(); got != want {
		t.Fatalf("defaultBudgetLimits() = %+v, want %+v", got, want)
	}

	// 修改一次调用的返回值，不能影响后续 Run 获取到的默认预算。
	first := defaultBudgetLimits()
	first.MaxSteps = 0
	if got := defaultBudgetLimits().MaxSteps; got != want.MaxSteps {
		t.Fatalf("defaultBudgetLimits().MaxSteps = %d, want %d", got, want.MaxSteps)
	}
}

// TestResolveEffectiveBudget 验证默认值、三级最小值、来源记录和非法配置边界。
func TestResolveEffectiveBudget(t *testing.T) {
	defaults := EffectiveBudget{
		Limits:          defaultBudgetLimits(),
		StepsSource:     BudgetScopeDefault,
		ToolCallsSource: BudgetScopeDefault,
		TokensSource:    BudgetScopeDefault,
		DurationSource:  BudgetScopeDefault,
	}
	tests := []struct {
		name    string          // 描述当前预算合并场景。
		set     BudgetSet       // 保存待合并的三级预算。
		want    EffectiveBudget // 保存期望的有效预算和来源。
		wantErr error           // 保存期望返回的稳定错误。
	}{
		{name: "empty set uses defaults", set: BudgetSet{}, want: defaults},
		{
			name: "looser values do not override defaults",
			set: BudgetSet{User: BudgetLimits{
				MaxSteps:       20,
				MaxToolCalls:   10,
				MaxTotalTokens: 200000,
				MaxDuration:    10 * time.Minute,
			}},
			want: defaults,
		},
		{
			name: "each dimension uses strictest scope",
			set: BudgetSet{
				User: BudgetLimits{
					MaxSteps:       20,
					MaxToolCalls:   2,
					MaxTotalTokens: 120000,
					MaxDuration:    10 * time.Minute,
				},
				Tenant: BudgetLimits{
					MaxSteps:       10,
					MaxToolCalls:   4,
					MaxTotalTokens: 80000,
					MaxDuration:    5 * time.Minute,
				},
				Run: BudgetLimits{
					MaxSteps:    5,
					MaxDuration: time.Minute,
				},
			},
			want: EffectiveBudget{
				Limits: BudgetLimits{
					MaxSteps:       5,
					MaxToolCalls:   2,
					MaxTotalTokens: 80000,
					MaxDuration:    time.Minute,
				},
				StepsSource:     BudgetScopeRun,
				ToolCallsSource: BudgetScopeUser,
				TokensSource:    BudgetScopeTenant,
				DurationSource:  BudgetScopeRun,
			},
		},
		{
			name: "equal values keep first scope",
			set: BudgetSet{
				User:   BudgetLimits{MaxSteps: 12},
				Tenant: BudgetLimits{MaxSteps: 12},
				Run:    BudgetLimits{MaxSteps: 12},
			},
			want: EffectiveBudget{
				Limits: BudgetLimits{
					MaxSteps:       12,
					MaxToolCalls:   defaults.Limits.MaxToolCalls,
					MaxTotalTokens: defaults.Limits.MaxTotalTokens,
					MaxDuration:    defaults.Limits.MaxDuration,
				},
				StepsSource:     BudgetScopeUser,
				ToolCallsSource: BudgetScopeDefault,
				TokensSource:    BudgetScopeDefault,
				DurationSource:  BudgetScopeDefault,
			},
		},
		{
			name:    "invalid scope is rejected",
			set:     BudgetSet{Run: BudgetLimits{MaxTotalTokens: -1}},
			want:    EffectiveBudget{},
			wantErr: ErrInvalidBudget,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveEffectiveBudget(tt.set)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("resolveEffectiveBudget() error = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("resolveEffectiveBudget() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestBudgetViolation 验证脱敏错误文本以及标准错误链中的类型和分类识别。
func TestBudgetViolation(t *testing.T) {
	tests := []struct {
		name      string          // 描述当前预算违规场景。
		violation BudgetViolation // 保存待验证的结构化违规信息。
		want      string          // 保存期望的完整脱敏错误文本。
	}{
		{
			name: "reason and scope",
			violation: BudgetViolation{
				Scope:  BudgetScopeRun,
				Reason: "max_steps_exceeded",
				Limit:  5,
				Used:   5,
			},
			want: "agent budget is exceeded: max_steps_exceeded scope=run",
		},
		{
			name:      "reason without scope",
			violation: BudgetViolation{Reason: "token_budget_exceeded"},
			want:      "agent budget is exceeded: token_budget_exceeded",
		},
		{
			name:      "empty violation",
			violation: BudgetViolation{},
			want:      "agent budget is exceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.violation.Error(); got != tt.want {
				t.Fatalf("BudgetViolation.Error() = %q, want %q", got, tt.want)
			}
			if !errors.Is(tt.violation, ErrBudgetExceeded) {
				t.Fatal("BudgetViolation should match ErrBudgetExceeded")
			}

			// 使用标准库错误链验证包装后仍能读取完整的结构化违规字段。
			wrapped := errors.Join(tt.violation)
			var got BudgetViolation
			if !errors.As(wrapped, &got) {
				t.Fatal("wrapped error should expose BudgetViolation")
			}
			if got != tt.violation {
				t.Fatalf("errors.As() = %+v, want %+v", got, tt.violation)
			}
		})
	}
}

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

// TestFixedBudgetResolver 验证接口实现、配置校验、取消传播和返回值隔离。
func TestFixedBudgetResolver(t *testing.T) {
	budgets := BudgetSet{
		User:   BudgetLimits{MaxSteps: 20},
		Tenant: BudgetLimits{MaxTotalTokens: 80000},
		Run:    BudgetLimits{MaxDuration: time.Minute},
	}
	var resolver BudgetResolver = FixedBudgetResolver{Budgets: budgets}
	got, err := resolver.Resolve(context.Background(), RunInput{})
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got != budgets {
		t.Fatalf("Resolve() = %+v, want %+v", got, budgets)
	}

	// BudgetSet 只含值字段，nil Context 也应能完成本地静态解析。
	fixed := FixedBudgetResolver{Budgets: budgets}
	returned, err := fixed.Resolve(nil, RunInput{})
	if err != nil {
		t.Fatalf("Resolve(nil context) error = %v", err)
	}
	returned.Run.MaxDuration = time.Second
	if fixed.Budgets.Run.MaxDuration != time.Minute {
		t.Fatalf("returned value modified resolver budget to %v", fixed.Budgets.Run.MaxDuration)
	}

	invalid := FixedBudgetResolver{Budgets: BudgetSet{
		Tenant: BudgetLimits{MaxSteps: -1},
	}}
	got, err = invalid.Resolve(context.Background(), RunInput{})
	if !errors.Is(err, ErrInvalidBudget) || got != (BudgetSet{}) {
		t.Fatalf("invalid Resolve() = (%+v, %v), want empty set and ErrInvalidBudget", got, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err = resolver.Resolve(ctx, RunInput{})
	if !errors.Is(err, context.Canceled) || got != (BudgetSet{}) {
		t.Fatalf("canceled Resolve() = (%+v, %v), want empty set and context.Canceled", got, err)
	}
}

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

// TestBudgetedRepairChargesEstimatedTokens 验证修复成功、失败和空修复函数的结算行为。
func TestBudgetedRepairChargesEstimatedTokens(t *testing.T) {
	t.Run("nil repair stays nil", func(t *testing.T) {
		if repair := budgetedRepair(newToolLoopTestExecution(t), nil); repair != nil {
			t.Fatal("budgetedRepair(nil) returned a non-nil function")
		}
	})

	t.Run("successful repair", func(t *testing.T) {
		execution := newToolLoopTestExecution(t)
		repair := budgetedRepair(execution, func(
			_ context.Context,
			_ structuredoutput.RepairRequest,
		) ([]byte, error) {
			return []byte("good"), nil
		})

		repaired, err := repair(context.Background(), structuredoutput.RepairRequest{Raw: []byte("bad")})
		if err != nil || string(repaired) != "good" {
			t.Fatalf("repair() = (%q, %v), want good output", repaired, err)
		}
		// 非空原文和修复结果各估算为 1 个 Token，并且修复计为一步。
		if execution.budget.totalTokens != 2 || execution.budget.estimatedTokens != 2 || execution.budget.steps != 1 {
			t.Fatalf("repair ledger = %+v, want 2 estimated tokens and 1 step", execution.budget)
		}
	})

	t.Run("failed repair still charges", func(t *testing.T) {
		expected := errors.New("repair unavailable")
		execution := newToolLoopTestExecution(t)
		repair := budgetedRepair(execution, func(
			_ context.Context,
			_ structuredoutput.RepairRequest,
		) ([]byte, error) {
			return []byte("part"), expected
		})

		_, err := repair(context.Background(), structuredoutput.RepairRequest{Raw: []byte("bad")})
		if !errors.Is(err, expected) {
			t.Fatalf("repair() error = %v, want original repair error", err)
		}
		if execution.budget.totalTokens != 2 || execution.budget.estimatedTokens != 2 || execution.budget.steps != 1 {
			t.Fatalf("failed repair ledger = %+v, want charged consumption", execution.budget)
		}
	})
}

// TestBudgetedRepairStopsAtStepAndTokenLimits 验证修复前拦截和超限后的真实估算账本。
func TestBudgetedRepairStopsAtStepAndTokenLimits(t *testing.T) {
	t.Run("step limit", func(t *testing.T) {
		started := time.Now()
		budget, err := newBudgetController(BudgetSet{Run: BudgetLimits{
			MaxSteps:    1,
			MaxDuration: time.Hour,
		}}, started)
		if err != nil {
			t.Fatalf("newBudgetController() error = %v", err)
		}
		execution := &runtimeExecution{started: started, budget: budget}
		calls := 0
		repair := budgetedRepair(execution, func(
			_ context.Context,
			_ structuredoutput.RepairRequest,
		) ([]byte, error) {
			calls++
			return []byte("fixed"), nil
		})

		if _, err := repair(context.Background(), structuredoutput.RepairRequest{Raw: []byte("bad")}); err != nil {
			t.Fatalf("first repair() error = %v", err)
		}
		_, err = repair(context.Background(), structuredoutput.RepairRequest{Raw: []byte("bad")})
		requireBudgetViolation(t, err, BudgetViolation{
			Scope:  BudgetScopeRun,
			Reason: "max_steps_exceeded",
			Limit:  1,
			Used:   1,
		})
		if calls != 1 {
			t.Fatalf("repair calls = %d, want 1", calls)
		}
	})

	t.Run("token limit", func(t *testing.T) {
		started := time.Now()
		budget, err := newBudgetController(BudgetSet{Run: BudgetLimits{
			MaxTotalTokens: 1,
			MaxDuration:    time.Hour,
		}}, started)
		if err != nil {
			t.Fatalf("newBudgetController() error = %v", err)
		}
		execution := &runtimeExecution{started: started, budget: budget}
		repair := budgetedRepair(execution, func(
			_ context.Context,
			_ structuredoutput.RepairRequest,
		) ([]byte, error) {
			return []byte("fixed"), nil
		})

		_, err = repair(context.Background(), structuredoutput.RepairRequest{Raw: []byte("bad")})
		requireBudgetViolation(t, err, BudgetViolation{
			Scope:  BudgetScopeRun,
			Reason: "token_budget_exceeded",
			Limit:  1,
			Used:   3,
		})
		if budget.totalTokens != 3 || budget.estimatedTokens != 3 {
			t.Fatalf("token ledger = (%d, %d), want 3 estimated tokens", budget.totalTokens, budget.estimatedTokens)
		}
	})
}

// TestBudgetedRepairRejectsInvalidExecutionAndCancellation 验证缺失账本和取消时不调用修复逻辑。
func TestBudgetedRepairRejectsInvalidExecutionAndCancellation(t *testing.T) {
	calls := 0
	repairFunc := func(
		_ context.Context,
		_ structuredoutput.RepairRequest,
	) ([]byte, error) {
		calls++
		return []byte("fixed"), nil
	}

	_, err := budgetedRepair(nil, repairFunc)(context.Background(), structuredoutput.RepairRequest{})
	if !errors.Is(err, ErrRuntimeNotConfigured) {
		t.Fatalf("repair() without execution error = %v, want ErrRuntimeNotConfigured", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = budgetedRepair(newToolLoopTestExecution(t), repairFunc)(ctx, structuredoutput.RepairRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled repair() error = %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Fatalf("repair calls = %d, want 0", calls)
	}
}

// TestEstimateTokenCount 验证空输入、向上取整、Unicode 和确定性估算规则。
func TestEstimateTokenCount(t *testing.T) {
	tests := []struct {
		name string // 描述当前估算场景。
		text string // 保存待估算的文本。
		want int    // 保存期望的本地估算 Token 数。
	}{
		{name: "empty", text: "", want: 0},
		{name: "one ASCII rune", text: "a", want: 1},
		{name: "four ASCII runes", text: "abcd", want: 1},
		{name: "five ASCII runes rounds up", text: "abcde", want: 2},
		{name: "four Chinese runes", text: "预算停止", want: 1},
		{name: "five Chinese runes", text: "预算停止了", want: 2},
		{name: "mixed Unicode", text: "ab预", want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := estimateTokenCount(tt.text); got != tt.want {
				t.Fatalf("estimateTokenCount(%q) = %d, want %d", tt.text, got, tt.want)
			}
			// 同一输入再次计算必须一致，不能依赖时间、随机数或供应商。
			if got := estimateTokenCount(tt.text); got != tt.want {
				t.Fatalf("second estimateTokenCount(%q) = %d, want %d", tt.text, got, tt.want)
			}
		})
	}
}
