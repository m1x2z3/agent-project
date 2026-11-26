package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

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
