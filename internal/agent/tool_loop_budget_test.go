package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"agent-project/internal/runstore"
	"agent-project/internal/runstore/memory"
	"agent-project/internal/structuredoutput"
	"agent-project/internal/tool/runner"
	"agent-project/internal/tool/tooling"
)

// newToolLoopForBudget 创建带独立预算解析器和真实工具执行边界的工具循环测试实例。
func newToolLoopForBudget(
	t *testing.T,
	model ModelCaller,
	budgets BudgetSet,
	store runstore.Store,
	analysisRepairs ...structuredoutput.RepairFunc,
) (*ToolLoopRuntime, *toolLoopTestTool) {
	t.Helper()
	tool := &toolLoopTestTool{
		definition: toolLoopExecutionDefinition(),
		result: tooling.ToolResult{
			Status: tooling.ResultSuccess,
			Data:   json.RawMessage(`{"hits":[]}`),
		},
	}
	runtime := newToolLoopRunStoreRuntime(t, model, tool, store, func(
		_ context.Context,
		input RunInput,
		decision structuredoutput.ToolDecision,
	) (runner.ToolCallRequest, error) {
		return validToolLoopCallRequest(input, decision), nil
	}, analysisRepairs...)
	runtime.budgetResolver = FixedBudgetResolver{Budgets: budgets}
	return runtime, tool
}

// TestToolLoopBudgetResolverAndUserIdentity 验证用户身份、非法预算和模型调用前置拦截。
func TestToolLoopBudgetResolverAndUserIdentity(t *testing.T) {
	model := &fakeRuntimeModel{}
	runtime, _ := newToolLoopForBudget(t, model, BudgetSet{
		User: BudgetLimits{MaxSteps: 1, MaxDuration: time.Hour},
	}, nil)
	input := toolLoopRunInput("run-tool-loop-budget-identity")

	_, err := runtime.Run(context.Background(), input)
	if !errors.Is(err, ErrInvalidRunInput) {
		t.Fatalf("Run() without user identity error = %v, want ErrInvalidRunInput", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model calls without user identity = %d, want 0", len(model.requests))
	}

	invalidRuntime, _ := newToolLoopForBudget(t, model, BudgetSet{
		Run: BudgetLimits{MaxSteps: -1},
	}, nil)
	_, err = invalidRuntime.Run(context.Background(), input)
	if !errors.Is(err, ErrInvalidBudget) {
		t.Fatalf("Run() with invalid budget error = %v, want ErrInvalidBudget", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model calls with invalid budget = %d, want 0", len(model.requests))
	}
}

// TestToolLoopStepBudgetStopsBeforeFinalModel 验证三级来源、模型步数和 RunStore 超预算终态。
func TestToolLoopStepBudgetStopsBeforeFinalModel(t *testing.T) {
	model := &fakeRuntimeModel{}
	store := memory.NewMemoryStore()
	runtime, _ := newToolLoopForBudget(t, model, BudgetSet{
		User: BudgetLimits{MaxSteps: 2, MaxDuration: time.Hour},
	}, store)
	input := toolLoopRunInput("run-tool-loop-step-budget")
	input.UserID = "user-a"

	_, err := runtime.Run(context.Background(), input)
	requireBudgetViolation(t, err, BudgetViolation{
		Scope:  BudgetScopeUser,
		Reason: "max_steps_exceeded",
		Limit:  2,
		Used:   2,
	})
	if len(model.requests) != 2 ||
		model.requests[0].Task != analysisTask ||
		model.requests[1].Task != toolDecisionTask {
		t.Fatalf("model requests = %+v, want analysis and first decision only", model.requests)
	}

	record, err := store.GetRun(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if record.Status != runstore.RunBudgetExceeded || record.Observation.ErrorKind != "max_steps_exceeded" {
		t.Fatalf("stored Run = %+v, want max steps budget terminal", record)
	}
	events, err := store.ListEvents(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	if len(events) != 4 || events[len(events)-1].Type != runstore.EventRunBudgetExceeded {
		t.Fatalf("events = %+v, want started, two model calls and budget terminal", events)
	}
}

// TestToolLoopTokenBudgetAcrossModelCalls 验证工具循环内跨模型调用的 Token 累计结算。
func TestToolLoopTokenBudgetAcrossModelCalls(t *testing.T) {
	tests := []struct {
		name           string // 描述当前 Token 预算场景。
		limit          int    // 保存 Run 累计 Token 上限。
		wantModelCalls int    // 保存超限前允许发生的模型调用次数。
		wantUsed       int64  // 保存触发终止时的真实累计 Token。
	}{
		{name: "exact limit stops first decision", limit: 3, wantModelCalls: 1, wantUsed: 3},
		{name: "second model exceeds limit", limit: 4, wantModelCalls: 2, wantUsed: 6},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := &fakeRuntimeModel{}
			store := memory.NewMemoryStore()
			runtime, _ := newToolLoopForBudget(t, model, BudgetSet{
				Run: BudgetLimits{MaxTotalTokens: tt.limit, MaxDuration: time.Hour},
			}, store)
			input := toolLoopRunInput("run-tool-loop-token-" + tt.name)

			_, err := runtime.Run(context.Background(), input)
			requireBudgetViolation(t, err, BudgetViolation{
				Scope:  BudgetScopeRun,
				Reason: "token_budget_exceeded",
				Limit:  int64(tt.limit),
				Used:   tt.wantUsed,
			})
			if len(model.requests) != tt.wantModelCalls {
				t.Fatalf("model calls = %d, want %d", len(model.requests), tt.wantModelCalls)
			}
			record, err := store.GetRun(context.Background(), input.RunID)
			if err != nil {
				t.Fatalf("GetRun() error = %v", err)
			}
			if record.Status != runstore.RunBudgetExceeded ||
				record.Observation.ErrorKind != "token_budget_exceeded" ||
				record.Observation.TotalTokens != int(tt.wantUsed) {
				t.Fatalf("stored Run = %+v, want actual token usage %d", record, tt.wantUsed)
			}
		})
	}
}

// TestToolLoopToolCallBudgetStopsBeforeExecutor 验证第二次工具调用在执行边界前被确定性终止。
func TestToolLoopToolCallBudgetStopsBeforeExecutor(t *testing.T) {
	model := &toolLoopSequenceModel{
		decisions: []string{
			`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"first"}}`,
			`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"second"}}`,
		},
	}
	store := memory.NewMemoryStore()
	runtime, tool := newToolLoopForBudget(t, model, BudgetSet{
		Run: BudgetLimits{MaxToolCalls: 1, MaxDuration: time.Hour},
	}, store)
	input := toolLoopRunInput("run-tool-loop-tool-budget")

	_, err := runtime.Run(context.Background(), input)
	requireBudgetViolation(t, err, BudgetViolation{
		Scope:  BudgetScopeRun,
		Reason: "max_tool_calls_exceeded",
		Limit:  1,
		Used:   1,
	})
	if !errors.Is(err, ErrMaxToolCallsExceeded) {
		t.Fatalf("Run() error = %v, want legacy ErrMaxToolCallsExceeded", err)
	}
	if tool.executeCalls != 1 {
		t.Fatalf("tool execute calls = %d, want 1", tool.executeCalls)
	}
	if len(model.requests) != 3 {
		t.Fatalf("model calls = %d, want analysis and two decisions", len(model.requests))
	}
	record, err := store.GetRun(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if record.Status != runstore.RunBudgetExceeded || record.Observation.ErrorKind != "max_tool_calls_exceeded" {
		t.Fatalf("stored Run = %+v, want tool budget terminal", record)
	}
	observation := record.Observation
	if observation.MaxToolCalls != 1 || observation.ToolCalls != 1 ||
		observation.ToolCallsSource != string(BudgetScopeRun) ||
		observation.MaxSteps != defaultBudgetLimits().MaxSteps || observation.Steps != 4 ||
		observation.StepsSource != string(BudgetScopeDefault) ||
		observation.BudgetTokens <= 0 || observation.EstimatedTokens != observation.BudgetTokens ||
		observation.TotalTokens != 0 {
		t.Fatalf("stored tool budget observation = %+v, want steps, calls and estimated source", observation)
	}
	events, err := store.ListEvents(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	terminal := events[len(events)-1]
	if terminal.Type != runstore.EventRunBudgetExceeded ||
		terminal.BudgetScope != string(BudgetScopeRun) ||
		terminal.BudgetLimit != 1 || terminal.BudgetUsed != 1 {
		t.Fatalf("terminal budget event = %+v, want run scope with limit and used", terminal)
	}
}

// TestToolLoopTotalTimeBudgetInterruptsModel 验证总时间 Cause 在失败收口前恢复为预算违规。
func TestToolLoopTotalTimeBudgetInterruptsModel(t *testing.T) {
	model := &blockingRuntimeModel{}
	store := memory.NewMemoryStore()
	runtime, _ := newToolLoopForBudget(t, model, BudgetSet{
		Run: BudgetLimits{MaxDuration: 20 * time.Millisecond},
	}, store)
	input := toolLoopRunInput("run-tool-loop-time-budget")

	_, err := runtime.Run(context.Background(), input)
	requireBudgetViolation(t, err, BudgetViolation{
		Scope:  BudgetScopeRun,
		Reason: "total_time_budget_exceeded",
		Limit:  20,
		Used:   20,
	})
	if model.request.Timeout <= 0 || model.request.Timeout > 20*time.Millisecond {
		t.Fatalf("model timeout = %v, want within Run remaining time", model.request.Timeout)
	}
	record, err := store.GetRun(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if record.Status != runstore.RunBudgetExceeded || record.Observation.ErrorKind != "total_time_budget_exceeded" {
		t.Fatalf("stored Run = %+v, want total time budget terminal", record)
	}
	if record.Observation.MaxDurationMS != 20 ||
		record.Observation.DurationSource != string(BudgetScopeRun) {
		t.Fatalf("stored time budget observation = %+v, want run duration limit and source", record.Observation)
	}
	events, err := store.ListEvents(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	terminal := events[len(events)-1]
	if terminal.Type != runstore.EventRunBudgetExceeded ||
		terminal.BudgetScope != string(BudgetScopeRun) ||
		terminal.BudgetLimit != 20 || terminal.BudgetUsed != 20 {
		t.Fatalf("terminal time event = %+v, want run scope and millisecond measurements", terminal)
	}
}

// TestToolLoopBudgetControllersAreConcurrentIndependent 验证共享 Runtime 的并发 Run 不共享消耗。
func TestToolLoopBudgetControllersAreConcurrentIndependent(t *testing.T) {
	model := &fakeRuntimeModel{}
	runtime, _ := newToolLoopForBudget(t, model, BudgetSet{
		Run: BudgetLimits{MaxSteps: 2, MaxDuration: time.Hour},
	}, nil)

	const runCount = 2
	errs := make(chan error, runCount)
	var wg sync.WaitGroup
	for index := 0; index < runCount; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			input := toolLoopRunInput("run-tool-loop-independent-" + string(rune('a'+index)))
			_, err := runtime.Run(context.Background(), input)
			errs <- err
		}(index)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		requireBudgetViolation(t, err, BudgetViolation{
			Scope:  BudgetScopeRun,
			Reason: "max_steps_exceeded",
			Limit:  2,
			Used:   2,
		})
	}
	// 每个 Run 都应独立完成分析和第一次工具决策，再被第二次模型步数检查拒绝。
	if len(model.requests) != runCount*2 {
		t.Fatalf("model calls across concurrent Runs = %d, want %d", len(model.requests), runCount*2)
	}
}

// TestToolLoopRepairUsesRunBudget 验证工具循环修复的计步、Token 结算和终态分类。
func TestToolLoopRepairUsesRunBudget(t *testing.T) {
	const invalidAnalysis = `{"summary":"","confidence":0.5}`
	const repairedAnalysis = `{"summary":"修复后","confidence":0.8}`

	t.Run("step budget stops before repair", func(t *testing.T) {
		model := &fakeRuntimeModel{analysisContent: invalidAnalysis}
		store := memory.NewMemoryStore()
		repairCalls := 0
		runtime, _ := newToolLoopForBudget(t, model, BudgetSet{
			Run: BudgetLimits{MaxSteps: 1, MaxDuration: time.Hour},
		}, store, func(context.Context, structuredoutput.RepairRequest) ([]byte, error) {
			repairCalls++
			return []byte(repairedAnalysis), nil
		})
		input := toolLoopRunInput("run-tool-loop-repair-step")

		_, err := runtime.Run(context.Background(), input)
		requireBudgetViolation(t, err, BudgetViolation{
			Scope:  BudgetScopeRun,
			Reason: "max_steps_exceeded",
			Limit:  1,
			Used:   1,
		})
		if repairCalls != 0 || len(model.requests) != 1 {
			t.Fatalf("repair calls = %d, model calls = %d, want 0 and 1", repairCalls, len(model.requests))
		}
		record, err := store.GetRun(context.Background(), input.RunID)
		if err != nil {
			t.Fatalf("GetRun() error = %v", err)
		}
		if record.Status != runstore.RunBudgetExceeded || record.Observation.ErrorKind != "max_steps_exceeded" {
			t.Fatalf("stored Run = %+v, want repair step budget terminal", record)
		}
	})

	t.Run("estimated tokens do not pollute provider usage", func(t *testing.T) {
		model := &fakeRuntimeModel{analysisContent: invalidAnalysis}
		store := memory.NewMemoryStore()
		repairCalls := 0
		runtime, _ := newToolLoopForBudget(t, model, BudgetSet{
			Run: BudgetLimits{MaxTotalTokens: 4, MaxDuration: time.Hour},
		}, store, func(context.Context, structuredoutput.RepairRequest) ([]byte, error) {
			repairCalls++
			return []byte(repairedAnalysis), nil
		})
		input := toolLoopRunInput("run-tool-loop-repair-token")

		_, err := runtime.Run(context.Background(), input)
		wantUsed := int64(3 + estimateTokenCount(invalidAnalysis) + estimateTokenCount(repairedAnalysis))
		requireBudgetViolation(t, err, BudgetViolation{
			Scope:  BudgetScopeRun,
			Reason: "token_budget_exceeded",
			Limit:  4,
			Used:   wantUsed,
		})
		if repairCalls != 1 || len(model.requests) != 1 {
			t.Fatalf("repair calls = %d, model calls = %d, want 1 and 1", repairCalls, len(model.requests))
		}
		record, err := store.GetRun(context.Background(), input.RunID)
		if err != nil {
			t.Fatalf("GetRun() error = %v", err)
		}
		if record.Status != runstore.RunBudgetExceeded ||
			record.Observation.ErrorKind != "token_budget_exceeded" ||
			record.Observation.TotalTokens != 3 {
			t.Fatalf("stored Run = %+v, want real provider usage 3", record)
		}
	})
}

// TestToolLoopAnalysisMissingUsageUsesEstimate 验证工具循环分析响应缺失 Usage 时不会绕过预算。
func TestToolLoopAnalysisMissingUsageUsesEstimate(t *testing.T) {
	model := &noUsageRuntimeModel{}
	store := memory.NewMemoryStore()
	runtime, _ := newToolLoopForBudget(t, model, BudgetSet{
		Run: BudgetLimits{MaxTotalTokens: 1, MaxDuration: time.Hour},
	}, store)
	input := toolLoopRunInput("run-tool-loop-analysis-missing-usage")

	_, err := runtime.Run(context.Background(), input)
	if len(model.requests) != 1 {
		t.Fatalf("model calls = %d, want only analysis", len(model.requests))
	}
	analysisContent := `{"summary":"定位订单服务错误","confidence":0.9}`
	wantUsed := int64(estimateTokenCount(systemSecurityInstruction) + estimateTokenCount(model.requests[0].Messages[1].Content) + estimateTokenCount(analysisContent))
	requireBudgetViolation(t, err, BudgetViolation{
		Scope:  BudgetScopeRun,
		Reason: "token_budget_exceeded",
		Limit:  1,
		Used:   wantUsed,
	})
	record, err := store.GetRun(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if record.Status != runstore.RunBudgetExceeded || record.Observation.TotalTokens != 0 {
		t.Fatalf("stored Run = %+v, want estimated budget and zero real usage", record)
	}
}

// TestToolLoopDecisionMissingUsageUsesEstimate 验证单轮工具决策缺失 Usage 时的估算结算。
func TestToolLoopDecisionMissingUsageUsesEstimate(t *testing.T) {
	model := &noUsageRuntimeModel{}
	started := time.Now()
	budget, err := newBudgetController(BudgetSet{Run: BudgetLimits{
		MaxTotalTokens: 1,
		MaxDuration:    time.Hour,
	}}, started)
	if err != nil {
		t.Fatalf("newBudgetController() error = %v", err)
	}
	execution := &runtimeExecution{started: started, budget: budget}
	runtime := &ToolLoopRuntime{model: model}
	const prompt = "请给出下一轮工具决策"
	const responseContent = `{"should_call":false}`

	_, err = runtime.callToolDecisionModel(context.Background(), toolLoopState{
		snapshot:           toolLoopSnapshot("v1"),
		execution:          execution,
		toolDecisionPrompt: prompt,
	})
	requireBudgetViolation(t, err, BudgetViolation{
		Scope:  BudgetScopeRun,
		Reason: "token_budget_exceeded",
		Limit:  1,
		Used:   int64(estimateTokenCount(systemSecurityInstruction) + estimateTokenCount(prompt) + estimateTokenCount(responseContent)),
	})
	if len(model.requests) != 1 || budget.steps != 1 {
		t.Fatalf("model calls = %d, steps = %d, want 1 and 1", len(model.requests), budget.steps)
	}
	if budget.totalTokens == 0 || budget.totalTokens != budget.estimatedTokens {
		t.Fatalf("token ledger = (%d, %d), want equal estimated consumption", budget.totalTokens, budget.estimatedTokens)
	}
	if execution.observation.TotalTokens != 0 {
		t.Fatalf("real observed tokens = %d, want 0", execution.observation.TotalTokens)
	}
}

// TestToolLoopFinalMissingUsageUsesEstimate 验证工具循环最终回答缺失 Usage 时的估算结算。
func TestToolLoopFinalMissingUsageUsesEstimate(t *testing.T) {
	model := &noUsageRuntimeModel{}
	started := time.Now()
	budget, err := newBudgetController(BudgetSet{Run: BudgetLimits{
		MaxTotalTokens: 1,
		MaxDuration:    time.Hour,
	}}, started)
	if err != nil {
		t.Fatalf("newBudgetController() error = %v", err)
	}
	execution := &runtimeExecution{started: started, budget: budget}
	runtime := &ToolLoopRuntime{model: model}
	const prompt = "请根据工具结果生成最终回答"
	const responseContent = `{"status":"answered","answer":"已定位"}`

	_, err = runtime.callToolLoopFinalModel(context.Background(), toolLoopState{
		snapshot:    toolLoopSnapshot("v1"),
		execution:   execution,
		finalPrompt: prompt,
	})
	requireBudgetViolation(t, err, BudgetViolation{
		Scope:  BudgetScopeRun,
		Reason: "token_budget_exceeded",
		Limit:  1,
		Used:   int64(estimateTokenCount(systemSecurityInstruction) + estimateTokenCount(prompt) + estimateTokenCount(responseContent)),
	})
	if len(model.requests) != 1 || budget.steps != 1 {
		t.Fatalf("model calls = %d, steps = %d, want 1 and 1", len(model.requests), budget.steps)
	}
	if budget.totalTokens == 0 || budget.totalTokens != budget.estimatedTokens {
		t.Fatalf("token ledger = (%d, %d), want equal estimated consumption", budget.totalTokens, budget.estimatedTokens)
	}
	if execution.observation.TotalTokens != 0 {
		t.Fatalf("real observed tokens = %d, want 0", execution.observation.TotalTokens)
	}
}

// TestToolLoopMaxTokensTracksRemainingBudget 验证分析、工具决策和最终回答逐次收紧输出上限。
func TestToolLoopMaxTokensTracksRemainingBudget(t *testing.T) {
	model := &fakeRuntimeModel{}
	runtime, _ := newToolLoopForBudget(t, model, BudgetSet{
		Run: BudgetLimits{MaxTotalTokens: 10, MaxDuration: time.Hour},
	}, nil)

	output, err := runtime.Run(context.Background(), toolLoopRunInput("run-tool-loop-max-tokens"))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if output.FinalAnswer.Answer != "已定位" || len(model.requests) != 3 {
		t.Fatalf("output = %+v, model requests = %+v", output, model.requests)
	}
	// 每次 Fake 调用真实消耗 3 Token，因此三次请求分别看到 10、7、4 的余额。
	for index, want := range []int{10, 7, 4} {
		request := model.requests[index]
		if request.MaxTokens == nil || *request.MaxTokens != want {
			t.Fatalf("request %d MaxTokens = %v, want %d", index, request.MaxTokens, want)
		}
	}
}
