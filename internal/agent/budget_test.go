package agent

import (
	"errors"
	"testing"
	"time"
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
