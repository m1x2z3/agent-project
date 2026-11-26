package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"agent-project/internal/configregistry"
	"agent-project/internal/modelgateway"
	"agent-project/internal/rollout"
	"agent-project/internal/runstore"
	"agent-project/internal/runstore/memory"
	"agent-project/internal/structuredoutput"
)

// newRuntimeForBudget 创建带可信预算解析器的普通 Runtime 测试实例。
func newRuntimeForBudget(
	t *testing.T,
	model ModelCaller,
	resolver BudgetResolver,
	store runstore.Store,
	analysisRepairs ...structuredoutput.RepairFunc,
) *Runtime {
	t.Helper()
	registry := configregistry.NewRegistry()
	publishRuntimeSnapshot(t, registry, "v1", "分析：{{.Query}}", "回答：{{.Query}} / {{.Analysis.Summary}}")
	if err := registry.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(registry, rollout.RolloutPolicy{
		CandidateVersion: "v1",
		Percentage:       100,
	})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	var analysisRepair structuredoutput.RepairFunc
	if len(analysisRepairs) > 0 {
		analysisRepair = analysisRepairs[0]
	}
	runtime, err := NewRuntime(RuntimeConfig{
		Registry:       registry,
		Selector:       selector,
		Model:          model,
		BudgetResolver: resolver,
		RunStore:       store,
		AnalysisRepair: analysisRepair,
	})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	return runtime
}

// TestRuntimeBudgetResolverAndUserIdentity 验证自定义解析器、用户身份和非法配置边界。
func TestRuntimeBudgetResolverAndUserIdentity(t *testing.T) {
	model := &fakeRuntimeModel{}
	resolver := FixedBudgetResolver{Budgets: BudgetSet{
		User: BudgetLimits{MaxSteps: 1, MaxDuration: time.Hour},
	}}
	runtime := newRuntimeForBudget(t, model, resolver, nil)
	input := RunInput{
		RequestID: "request-budget-identity",
		TenantID:  "tenant-a",
		RunID:     "run-budget-identity",
		Query:     "问题",
	}

	_, err := runtime.Run(context.Background(), input)
	if !errors.Is(err, ErrInvalidRunInput) {
		t.Fatalf("Run() without user identity error = %v, want ErrInvalidRunInput", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model calls without user identity = %d, want 0", len(model.requests))
	}

	invalidRuntime := newRuntimeForBudget(t, model, FixedBudgetResolver{Budgets: BudgetSet{
		Run: BudgetLimits{MaxSteps: -1},
	}}, nil)
	_, err = invalidRuntime.Run(context.Background(), input)
	if !errors.Is(err, ErrInvalidBudget) {
		t.Fatalf("Run() with invalid budget error = %v, want ErrInvalidBudget", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model calls with invalid budget = %d, want 0", len(model.requests))
	}
}

// TestRuntimeStepBudgetStopsBeforeFinalModel 验证用户级步数限制和 RunStore 终态。
func TestRuntimeStepBudgetStopsBeforeFinalModel(t *testing.T) {
	model := &fakeRuntimeModel{}
	store := memory.NewMemoryStore()
	runtime := newRuntimeForBudget(t, model, FixedBudgetResolver{Budgets: BudgetSet{
		User: BudgetLimits{MaxSteps: 1, MaxDuration: time.Hour},
	}}, store)
	input := RunInput{
		RequestID: "request-step-budget",
		TenantID:  "tenant-a",
		RunID:     "run-step-budget",
		UserID:    "user-a",
		Query:     "问题",
	}

	_, err := runtime.Run(context.Background(), input)
	requireBudgetViolation(t, err, BudgetViolation{
		Scope:  BudgetScopeUser,
		Reason: "max_steps_exceeded",
		Limit:  1,
		Used:   1,
	})
	if len(model.requests) != 1 || model.requests[0].Task != analysisTask {
		t.Fatalf("model requests = %+v, want only analysis", model.requests)
	}

	record, err := store.GetRun(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if record.Status != runstore.RunBudgetExceeded || record.Observation.ErrorKind != "max_steps_exceeded" {
		t.Fatalf("stored Run = %+v, want max steps budget terminal", record)
	}
	observation := record.Observation
	if observation.MaxSteps != 1 || observation.Steps != 1 ||
		observation.StepsSource != string(BudgetScopeUser) ||
		observation.MaxToolCalls != defaultBudgetLimits().MaxToolCalls ||
		observation.ToolCallsSource != string(BudgetScopeDefault) ||
		observation.MaxTotalTokens != int64(defaultBudgetLimits().MaxTotalTokens) ||
		observation.TokensSource != string(BudgetScopeDefault) ||
		observation.MaxDurationMS != defaultBudgetLimits().MaxDuration.Milliseconds() ||
		observation.DurationSource != string(BudgetScopeDefault) ||
		observation.BudgetTokens != 3 || observation.EstimatedTokens != 0 ||
		observation.TotalTokens != 3 {
		t.Fatalf("stored budget observation = %+v, want effective limits, consumption and sources", observation)
	}
	events, err := store.ListEvents(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	if len(events) != 3 || events[len(events)-1].Type != runstore.EventRunBudgetExceeded {
		t.Fatalf("events = %+v, want started, model call and budget terminal", events)
	}
	terminal := events[len(events)-1]
	if terminal.BudgetScope != string(BudgetScopeUser) ||
		terminal.BudgetLimit != 1 || terminal.BudgetUsed != 1 {
		t.Fatalf("terminal budget event = %+v, want user scope with limit and used", terminal)
	}
}

// TestRuntimeTokenBudgetAcrossModelCalls 验证累计结算和达到上限后的模型拦截。
func TestRuntimeTokenBudgetAcrossModelCalls(t *testing.T) {
	tests := []struct {
		name           string // 描述当前 Token 预算场景。
		limit          int    // 保存 Run 累计 Token 上限。
		wantModelCalls int    // 保存超限前允许发生的模型调用次数。
		wantUsed       int64  // 保存触发终止时的累计 Token。
	}{
		{name: "exact limit stops next model", limit: 3, wantModelCalls: 1, wantUsed: 3},
		{name: "second model can exceed limit", limit: 5, wantModelCalls: 2, wantUsed: 6},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := &fakeRuntimeModel{}
			store := memory.NewMemoryStore()
			runtime := newRuntimeForBudget(t, model, FixedBudgetResolver{Budgets: BudgetSet{
				Run: BudgetLimits{MaxTotalTokens: tt.limit, MaxDuration: time.Hour},
			}}, store)
			input := RunInput{
				RequestID: "request-token-" + tt.name,
				TenantID:  "tenant-a",
				RunID:     "run-token-" + tt.name,
				Query:     "问题",
			}

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
			// 测试快照未配置路由 MaxTokens，因此分析请求应使用当时的全部剩余额度。
			if model.requests[0].MaxTokens == nil || *model.requests[0].MaxTokens != tt.limit {
				t.Fatalf("analysis MaxTokens = %v, want remaining budget %d", model.requests[0].MaxTokens, tt.limit)
			}
			if tt.wantModelCalls == 2 {
				// 分析模型已真实消耗 3 Token，最终回答只能使用剩余的 limit-3。
				wantRemaining := tt.limit - 3
				if model.requests[1].MaxTokens == nil || *model.requests[1].MaxTokens != wantRemaining {
					t.Fatalf("final MaxTokens = %v, want remaining budget %d", model.requests[1].MaxTokens, wantRemaining)
				}
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

// TestRuntimeMaxTokensKeepsSmallerRouteLimit 验证剩余预算宽松时不会放大路由上限。
func TestRuntimeMaxTokensKeepsSmallerRouteLimit(t *testing.T) {
	model := &fakeRuntimeModel{}
	snapshot := runtimeSnapshot("v1", "分析：{{.Query}}", "回答：{{.Query}}")
	route := snapshot.Routes[analysisTask]
	route.MaxTokens = 2
	snapshot.Routes[analysisTask] = route
	runtime := &Runtime{model: model}

	_, err := runtime.callAnalysisModel(context.Background(), runtimeState{
		snapshot:       snapshot,
		execution:      newToolLoopTestExecution(t),
		analysisPrompt: "分析：问题",
	})
	if err != nil {
		t.Fatalf("callAnalysisModel() error = %v", err)
	}
	if len(model.requests) != 1 || model.requests[0].MaxTokens == nil || *model.requests[0].MaxTokens != 2 {
		t.Fatalf("analysis requests = %+v, want route MaxTokens 2", model.requests)
	}
}

// blockingRuntimeModel 阻塞到 Run Context 结束，用于验证总时间预算中断模型调用。
type blockingRuntimeModel struct {
	request modelgateway.ChatRequest // 保存实际收到的单次模型请求参数。
}

// Chat 记录请求并等待 Context 取消，不访问外部模型服务。
func (m *blockingRuntimeModel) Chat(ctx context.Context, request modelgateway.ChatRequest) (modelgateway.ChatResponse, error) {
	m.request = request
	<-ctx.Done()
	return modelgateway.ChatResponse{}, ctx.Err()
}

// TestRuntimeTotalTimeBudgetInterruptsModel 验证总时间到期、单次超时收口和终态记录。
func TestRuntimeTotalTimeBudgetInterruptsModel(t *testing.T) {
	model := &blockingRuntimeModel{}
	store := memory.NewMemoryStore()
	runtime := newRuntimeForBudget(t, model, FixedBudgetResolver{Budgets: BudgetSet{
		Run: BudgetLimits{MaxDuration: 20 * time.Millisecond},
	}}, store)
	input := RunInput{
		RequestID: "request-time-budget",
		TenantID:  "tenant-a",
		RunID:     "run-time-budget",
		Query:     "问题",
	}

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
}

// TestRuntimeBudgetControllersAreIndependent 验证共享 Runtime 不会共享不同 Run 的消耗。
func TestRuntimeBudgetControllersAreIndependent(t *testing.T) {
	model := &fakeRuntimeModel{}
	runtime := newRuntimeForBudget(t, model, FixedBudgetResolver{Budgets: BudgetSet{
		Run: BudgetLimits{MaxSteps: 1, MaxDuration: time.Hour},
	}}, nil)

	for _, runID := range []string{"run-budget-independent-1", "run-budget-independent-2"} {
		_, err := runtime.Run(context.Background(), RunInput{
			RequestID: "request-" + runID,
			TenantID:  "tenant-a",
			RunID:     runID,
			Query:     "问题",
		})
		requireBudgetViolation(t, err, BudgetViolation{
			Scope:  BudgetScopeRun,
			Reason: "max_steps_exceeded",
			Limit:  1,
			Used:   1,
		})
	}
	// 每个 Run 都应各执行一次分析模型，而不是第二个 Run 继承第一个 Run 的消耗。
	if len(model.requests) != 2 {
		t.Fatalf("model calls across two Runs = %d, want 2", len(model.requests))
	}
}

// TestRuntimeRepairUsesRunBudget 验证修复计步、估算结算和真实 Usage 的隔离。
func TestRuntimeRepairUsesRunBudget(t *testing.T) {
	const invalidAnalysis = `{"summary":"","confidence":0.5}`
	const repairedAnalysis = `{"summary":"修复后","confidence":0.8}`

	t.Run("step budget stops before repair", func(t *testing.T) {
		model := &fakeRuntimeModel{analysisContent: invalidAnalysis}
		store := memory.NewMemoryStore()
		repairCalls := 0
		runtime := newRuntimeForBudget(t, model, FixedBudgetResolver{Budgets: BudgetSet{
			Run: BudgetLimits{MaxSteps: 1, MaxDuration: time.Hour},
		}}, store, func(context.Context, structuredoutput.RepairRequest) ([]byte, error) {
			repairCalls++
			return []byte(repairedAnalysis), nil
		})
		input := RunInput{RequestID: "request-repair-step", TenantID: "tenant-a", RunID: "run-repair-step", Query: "问题"}

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

	t.Run("estimated repair tokens preserve real observation", func(t *testing.T) {
		model := &fakeRuntimeModel{analysisContent: invalidAnalysis}
		store := memory.NewMemoryStore()
		repairCalls := 0
		runtime := newRuntimeForBudget(t, model, FixedBudgetResolver{Budgets: BudgetSet{
			Run: BudgetLimits{MaxTotalTokens: 4, MaxDuration: time.Hour},
		}}, store, func(context.Context, structuredoutput.RepairRequest) ([]byte, error) {
			repairCalls++
			return []byte(repairedAnalysis), nil
		})
		input := RunInput{RequestID: "request-repair-token", TenantID: "tenant-a", RunID: "run-repair-token", Query: "问题"}

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
		// 分析模型真实 Usage 为 3，Repair 估算只能进入预算账本，不能污染该字段。
		if record.Status != runstore.RunBudgetExceeded ||
			record.Observation.ErrorKind != "token_budget_exceeded" ||
			record.Observation.TotalTokens != 3 {
			t.Fatalf("stored Run = %+v, want real provider usage 3", record)
		}
	})
}

// noUsageRuntimeModel 保留确定性模型响应，但模拟供应商完全缺失 Usage。
type noUsageRuntimeModel struct {
	fakeRuntimeModel
}

// Chat 复用普通 Fake 的请求记录和响应，再清除 Usage 以触发本地估算。
func (m *noUsageRuntimeModel) Chat(
	ctx context.Context,
	request modelgateway.ChatRequest,
) (modelgateway.ChatResponse, error) {
	response, err := m.fakeRuntimeModel.Chat(ctx, request)
	response.Usage = nil
	return response, err
}

// TestRuntimeMissingUsageUsesEstimatedTokenBudget 验证缺失 Usage 不会绕过分析阶段预算。
func TestRuntimeMissingUsageUsesEstimatedTokenBudget(t *testing.T) {
	const analysisContent = `{"summary":"定位订单服务错误","confidence":0.9}`
	analysisEstimate := estimateTokenCount("分析：问题") + estimateTokenCount(analysisContent)
	tests := []struct {
		name  string // 描述当前缺失 Usage 的预算场景。
		limit int    // 保存当前 Run 的 Token 上限。
	}{
		{name: "estimate reaches exact limit", limit: analysisEstimate},
		{name: "estimate exceeds limit", limit: analysisEstimate - 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := &noUsageRuntimeModel{}
			store := memory.NewMemoryStore()
			runtime := newRuntimeForBudget(t, model, FixedBudgetResolver{Budgets: BudgetSet{
				Run: BudgetLimits{MaxTotalTokens: tt.limit, MaxDuration: time.Hour},
			}}, store)
			input := RunInput{
				RequestID: "request-missing-usage",
				TenantID:  "tenant-a",
				RunID:     "run-missing-usage",
				Query:     "问题",
			}

			_, err := runtime.Run(context.Background(), input)
			requireBudgetViolation(t, err, BudgetViolation{
				Scope:  BudgetScopeRun,
				Reason: "token_budget_exceeded",
				Limit:  int64(tt.limit),
				Used:   int64(analysisEstimate),
			})
			if len(model.requests) != 1 {
				t.Fatalf("model calls = %d, want only analysis", len(model.requests))
			}
			record, err := store.GetRun(context.Background(), input.RunID)
			if err != nil {
				t.Fatalf("GetRun() error = %v", err)
			}
			// 估算只用于预算，RunStore 中供应商真实 TotalTokens 仍为零。
			if record.Status != runstore.RunBudgetExceeded || record.Observation.TotalTokens != 0 {
				t.Fatalf("stored Run = %+v, want zero real usage", record)
			}
			observation := record.Observation
			if observation.MaxTotalTokens != int64(tt.limit) ||
				observation.BudgetTokens != int64(analysisEstimate) ||
				observation.EstimatedTokens != int64(analysisEstimate) ||
				observation.TokensSource != string(BudgetScopeRun) || observation.Steps != 1 {
				t.Fatalf("stored estimated budget observation = %+v, want separate estimated consumption", observation)
			}
			events, err := store.ListEvents(context.Background(), input.RunID)
			if err != nil {
				t.Fatalf("ListEvents() error = %v", err)
			}
			terminal := events[len(events)-1]
			if terminal.Type != runstore.EventRunBudgetExceeded ||
				terminal.BudgetScope != string(BudgetScopeRun) ||
				terminal.BudgetLimit != int64(tt.limit) ||
				terminal.BudgetUsed != int64(analysisEstimate) {
				t.Fatalf("terminal token event = %+v, want run scope with estimate consumption", terminal)
			}
		})
	}
}

// TestRuntimeFinalMissingUsageUsesEstimatedTokenBudget 验证最终回答的估算消耗和恰好达限语义。
func TestRuntimeFinalMissingUsageUsesEstimatedTokenBudget(t *testing.T) {
	const analysisContent = `{"summary":"定位订单服务错误","confidence":0.9}`
	const finalContent = `{"status":"answered","answer":"已定位"}`
	analysisEstimate := estimateTokenCount("分析：问题") + estimateTokenCount(analysisContent)
	finalEstimate := estimateTokenCount("回答：问题 / 定位订单服务错误") + estimateTokenCount(finalContent)
	totalEstimate := analysisEstimate + finalEstimate

	tests := []struct {
		name      string // 描述最终回答的估算预算场景。
		limit     int    // 保存当前 Run 的 Token 上限。
		wantError bool   // 标识最终结算是否应超过预算。
	}{
		{name: "exact limit allows completion", limit: totalEstimate, wantError: false},
		{name: "final estimate exceeds limit", limit: totalEstimate - 1, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model := &noUsageRuntimeModel{}
			store := memory.NewMemoryStore()
			runtime := newRuntimeForBudget(t, model, FixedBudgetResolver{Budgets: BudgetSet{
				Run: BudgetLimits{MaxTotalTokens: tt.limit, MaxDuration: time.Hour},
			}}, store)
			input := RunInput{
				RequestID: "request-final-missing-usage",
				TenantID:  "tenant-a",
				RunID:     "run-final-missing-usage",
				Query:     "问题",
			}

			_, err := runtime.Run(context.Background(), input)
			if tt.wantError {
				requireBudgetViolation(t, err, BudgetViolation{
					Scope:  BudgetScopeRun,
					Reason: "token_budget_exceeded",
					Limit:  int64(tt.limit),
					Used:   int64(totalEstimate),
				})
			} else if err != nil {
				t.Fatalf("Run() at exact limit error = %v", err)
			}
			if len(model.requests) != 2 {
				t.Fatalf("model calls = %d, want analysis and final answer", len(model.requests))
			}
			record, err := store.GetRun(context.Background(), input.RunID)
			if err != nil {
				t.Fatalf("GetRun() error = %v", err)
			}
			wantStatus := runstore.RunSucceeded
			if tt.wantError {
				wantStatus = runstore.RunBudgetExceeded
			}
			if record.Status != wantStatus || record.Observation.TotalTokens != 0 {
				t.Fatalf("stored Run = %+v, want status %q and zero real usage", record, wantStatus)
			}
		})
	}
}
