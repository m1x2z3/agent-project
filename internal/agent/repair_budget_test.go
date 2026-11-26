package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-project/internal/structuredoutput"
)

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
