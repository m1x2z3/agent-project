package agent

import (
	"context"
	"fmt"
	"strings"

	"agent-project/internal/recording"
	"agent-project/internal/structuredoutput"
	"github.com/cloudwego/eino/compose"
)

// NewRuntime 构建当天的类型化 Eino Chain。
func NewRuntime(cfg RuntimeConfig) (*Runtime, error) {
	// Runtime 通过依赖注入连接配置注册中心、灰度选择器和模型调用器。
	// 这样业务编排不依赖具体供应商，也便于使用 Fake 进行离线测试。
	if cfg.Registry == nil || cfg.Selector == nil || cfg.Model == nil {
		return nil, ErrRuntimeNotConfigured
	}
	observer := cfg.Observer
	if observer == nil {
		observer = noopObserver{}
	}
	// Repair 在构造时绑定一次，实际执行时再从 Context 获取当前 Run 的独立账本。
	// 保留原始回调变量，避免包装后的闭包意外递归调用自身。
	analysisRepair := cfg.AnalysisRepair
	if analysisRepair != nil {
		original := analysisRepair
		analysisRepair = func(
			ctx context.Context,
			request structuredoutput.RepairRequest,
		) ([]byte, error) {
			return budgetedRepair(executionFromContext(ctx), original)(ctx, request)
		}
	}
	finalAnswerRepair := cfg.FinalAnswerRepair
	if finalAnswerRepair != nil {
		original := finalAnswerRepair
		finalAnswerRepair = func(
			ctx context.Context,
			request structuredoutput.RepairRequest,
		) ([]byte, error) {
			return budgetedRepair(executionFromContext(ctx), original)(ctx, request)
		}
	}
	runtime := &Runtime{
		registry:          cfg.Registry,
		selector:          cfg.Selector,
		model:             cfg.Model,
		budgetResolver:    cfg.BudgetResolver,
		observer:          observer,
		runStore:          cfg.RunStore,
		onStoreError:      cfg.OnStoreError,
		analysisRepair:    analysisRepair,
		finalAnswerRepair: finalAnswerRepair,
	}
	chain, err := runtime.buildChain(context.Background())
	if err != nil {
		return nil, fmt.Errorf("build agent runtime chain: %w", err)
	}
	runtime.chain = chain
	return runtime, nil
}

// Run 执行一次正常请求，灰度选择器决定使用 active 或候选版本。
func (r *Runtime) Run(ctx context.Context, input RunInput) (RunOutput, error) {
	return r.run(ctx, input, false)
}

// RunShadow 强制执行候选版本，只进行模型调用和结构化解析。
//
// 当前 Runtime 没有工具或副作用依赖，因此影子运行不会改变主请求结果，
// 也不会执行写操作或访问数据库。
func (r *Runtime) RunShadow(ctx context.Context, input RunInput) (RunOutput, error) {
	return r.run(ctx, input, true)
}

func (r *Runtime) run(ctx context.Context, input RunInput, shadow bool) (RunOutput, error) {
	if r == nil || r.chain == nil {
		return RunOutput{}, ErrRuntimeNotConfigured
	}
	if err := validateRunInput(input); err != nil {
		return RunOutput{}, err
	}

	started := recording.ClockFromContext(ctx).Now()
	budgetSet := BudgetSet{}
	var err error
	if r.budgetResolver != nil {
		budgetSet, err = r.budgetResolver.Resolve(ctx, input)
		if err != nil {
			return RunOutput{}, err
		}
	}
	budget, err := newBudgetController(budgetSet, started)
	if err != nil {
		return RunOutput{}, err
	}
	// 只有配置用户级预算时才强制要求可信用户身份，保持现有无用户预算调用兼容。
	if budgetSet.User != (BudgetLimits{}) && strings.TrimSpace(input.UserID) == "" {
		return RunOutput{}, ErrInvalidRunInput
	}
	budgetCause := BudgetViolation{
		Scope:  budget.effective.DurationSource,
		Reason: "total_time_budget_exceeded",
		Limit:  budget.effective.Limits.MaxDuration.Milliseconds(),
		Used:   budget.effective.Limits.MaxDuration.Milliseconds(),
	}
	runCtx, cancel := context.WithTimeoutCause(ctx, budget.effective.Limits.MaxDuration, budgetCause)
	defer cancel()

	execution := &runtimeExecution{
		started:  started,
		budget:   budget,
		shadow:   shadow,
		recorder: &runStoreRecorder{store: r.runStore, onError: r.onStoreError},
		observation: RunObservation{
			RequestID: input.RequestID,
			TenantID:  input.TenantID,
			RunID:     input.RunID,
			Shadow:    shadow,
		},
	}
	// 将本次执行状态放入 Context，供 Chain 中的各阶段共享，
	// 同时继续沿用调用方 Context 和 Run 总时间预算的取消信号。
	runCtx = context.WithValue(runCtx, executionContextKey{}, execution)
	if shadow {
		// 影子运行固定使用候选版本，但不改变 Registry 的 active 指针。
		runCtx = context.WithValue(runCtx, forcedReleaseContextKey{}, r.selectorCandidateVersion())
	}
	output, err := r.chain.Invoke(runCtx, input)
	// 总时间 Context 到期时，将供应商或阶段返回的通用 deadline 错误统一
	// 转换为带原因和来源的预算违规；调用方自身的 deadline 不会被改写。
	if runCtx.Err() == context.DeadlineExceeded {
		if violation, ok := context.Cause(runCtx).(BudgetViolation); ok {
			output = RunOutput{}
			err = runtimeFailure("run_budget", violation)
		}
	}
	r.observeExecution(runCtx, input, execution, output, err)
	return output, err
}

func (r *Runtime) buildChain(ctx context.Context) (compose.Runnable[RunInput, RunOutput], error) {
	chain := compose.NewChain[RunInput, RunOutput]()
	// 这些阶段存在明确的数据依赖，Append 顺序就是实际执行顺序：
	// 必须先选版本并绑定快照，再渲染 Prompt、调用模型、校验结果，最后组装输出。
	// 例如没有完成 bindRunSnapshot，后续阶段就不能安全读取 Prompt、Schema 和路由版本。
	chain.
		AppendLambda(compose.InvokableLambda(r.selectRelease)).
		AppendLambda(compose.InvokableLambda(r.bindRunSnapshot)).
		AppendLambda(compose.InvokableLambda(r.renderAnalysisPrompt)).
		AppendLambda(compose.InvokableLambda(r.callAnalysisModel)).
		AppendLambda(compose.InvokableLambda(r.decodeAnalysis)).
		AppendLambda(compose.InvokableLambda(r.renderFinalPrompt)).
		AppendLambda(compose.InvokableLambda(r.callFinalModel)).
		AppendLambda(compose.InvokableLambda(r.decodeFinalAnswer)).
		AppendLambda(compose.InvokableLambda(r.formatRunOutput))
	return chain.Compile(ctx)
}
