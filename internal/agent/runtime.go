package agent

import (
	"context"
	"fmt"
	"strings"

	"agent-project/internal/configregistry"
	"agent-project/internal/rollout"
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

	// 共享执行准备统一处理预算、总超时、执行账本和 Shadow 观测标记。
	runCtx, cancel, execution, err := prepareRunExecution(ctx, input, runExecutionOptions{
		budgetResolver: r.budgetResolver,
		runStore:       r.runStore,
		onStoreError:   r.onStoreError,
		shadow:         shadow,
	})
	if err != nil {
		return RunOutput{}, err
	}
	defer cancel()
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

func (r *Runtime) selectRelease(ctx context.Context, input RunInput) (runtimeState, error) {
	if err := ctx.Err(); err != nil {
		return runtimeState{}, err
	}
	execution := executionFromContext(ctx)
	if execution == nil {
		return runtimeState{}, ErrRuntimeNotConfigured
	}
	// 这里只做版本选择，不加载完整配置内容。
	// 完整快照会在下一阶段绑定，从而把本次 Run 使用的配置固定下来。
	decision, err := r.selector.Select(rollout.SelectionInput{
		RequestID: input.RequestID,
		TenantID:  input.TenantID,
	})
	if err != nil {
		return runtimeState{}, err
	}
	if forced, ok := ctx.Value(forcedReleaseContextKey{}).(string); ok && strings.TrimSpace(forced) != "" {
		decision.ReleaseVersion = forced
		decision.Candidate = true
		decision.Reason = "shadow_candidate"
	}
	execution.mu.Lock()
	execution.observation.ReleaseVersion = decision.ReleaseVersion
	execution.mu.Unlock()
	return runtimeState{input: input, execution: execution}, nil
}

func (r *Runtime) bindRunSnapshot(ctx context.Context, state runtimeState) (runtimeState, error) {
	if err := ctx.Err(); err != nil {
		return runtimeState{}, err
	}
	// 将灰度选择结果转换成不可变的 Run 快照。
	// 后续即使 active 版本发生变化，本次执行仍使用同一份配置。
	var (
		snapshot configregistry.RunSnapshot
		err      error
	)
	if state.execution.shadow {
		// 影子运行只读取候选版本，不写入 Registry 的 Run 绑定表，
		// 避免长期运行后产生大量无业务意义的临时 Run 记录。
		var configSnapshot configregistry.ConfigSnapshot
		configSnapshot, err = r.registry.Get(state.execution.observation.ReleaseVersion)
		snapshot = configregistry.RunSnapshot{
			RunID:          state.input.RunID,
			ReleaseVersion: state.execution.observation.ReleaseVersion,
			Snapshot:       configSnapshot,
		}
	} else {
		snapshot, err = r.registry.BindRunToVersion(
			state.input.RunID,
			state.execution.observation.ReleaseVersion,
		)
	}
	if err != nil {
		return runtimeState{}, runtimeFailure("bind_run", err)
	}
	state.snapshot = snapshot.Snapshot
	if !state.execution.shadow {
		// 普通 Run 在快照绑定成功后才创建持久化记录，确保 RunStore 保存的
		// 配置版本与后续模型调用使用的是同一份不可变快照。
		state.execution.recorder.recordRunStarted(
			ctx,
			state.input,
			state.snapshot,
			state.execution.started,
		)
	}
	return state, nil
}

func (r *Runtime) renderAnalysisPrompt(ctx context.Context, state runtimeState) (runtimeState, error) {
	if err := ctx.Err(); err != nil {
		return runtimeState{}, err
	}
	// 分类 Prompt 必须从 Run 快照读取，不能在 Runtime 中硬编码，
	// 这样 Prompt 版本才能与本次 Run 的发布版本保持一致。
	promptVersion, err := promptFromSnapshot(state.snapshot, analysisPromptName)
	if err != nil {
		return runtimeState{}, err
	}
	rendered, err := renderPrompt(promptVersion.Template, map[string]any{"Query": state.input.Query})
	if err != nil {
		return runtimeState{}, err
	}
	state.analysisPrompt = rendered
	return state, nil
}

func (r *Runtime) callAnalysisModel(ctx context.Context, state runtimeState) (runtimeState, error) {
	route, err := routeFromSnapshot(state.snapshot, analysisTask)
	if err != nil {
		return runtimeState{}, err
	}
	// 共享调用内核统一执行预算、记录和 Token 结算，这里保留分析阶段错误分类。
	response, err := r.callModelWithinBudget(ctx, state, route, runtimeModelCall{
		task:             analysisTask,
		prompt:           state.analysisPrompt,
		budgetStage:      "analysis_budget",
		tokenBudgetStage: "analysis_token_budget",
		modelStage:       "analysis_model",
		modelError:       ErrAnalysisModelFailed,
	})
	state.analysisResponse = response
	if err != nil {
		return runtimeState{}, err
	}
	return state, nil
}

func (r *Runtime) decodeAnalysis(ctx context.Context, state runtimeState) (runtimeState, error) {
	references, err := resolveReferences(state.snapshot, analysisPromptName, analysisTask, analysisTask)
	if err != nil {
		return runtimeState{}, err
	}
	metadata := structuredoutput.ExecutionMetadata{
		Provider:           references.Provider,
		Model:              references.Model,
		PromptVersion:      references.PromptVersion,
		SchemaVersion:      references.SchemaVersion,
		ModelConfigVersion: references.ModelConfigVersion,
	}
	// 模型输出必须经过结构化输出内核的 Schema 和业务语义校验；
	// Repair 由调用方注入，并且最多执行一次，避免无界重试。
	value, report, err := structuredoutput.DecodeTaskAnalysis(ctx, []byte(state.analysisResponse.Message.Content), structuredoutput.DecodeOptions{
		Metadata: metadata,
		Repair:   r.analysisRepair,
	})
	if err != nil {
		return runtimeState{}, runtimeFailure("analysis_decode", fmt.Errorf("%w: %w", ErrAnalysisDecodeFailed, err))
	}
	state.analysis = value
	state.analysisReport = report
	return state, nil
}

func (r *Runtime) renderFinalPrompt(ctx context.Context, state runtimeState) (runtimeState, error) {
	if err := ctx.Err(); err != nil {
		return runtimeState{}, err
	}
	// 最终回答阶段同时使用原始问题和已校验的分析结果，
	// 避免把未经校验的模型原文直接拼进下一阶段。
	promptVersion, err := promptFromSnapshot(state.snapshot, finalPromptName)
	if err != nil {
		return runtimeState{}, err
	}
	rendered, err := renderPrompt(promptVersion.Template, map[string]any{
		"Query":    state.input.Query,
		"Analysis": state.analysis,
	})
	if err != nil {
		return runtimeState{}, err
	}
	state.finalPrompt = rendered
	return state, nil
}

func (r *Runtime) callFinalModel(ctx context.Context, state runtimeState) (runtimeState, error) {
	route, err := routeFromSnapshot(state.snapshot, finalTask)
	if err != nil {
		return runtimeState{}, err
	}
	// 共享调用内核统一执行预算、记录和 Token 结算，这里保留最终回答阶段错误分类。
	response, err := r.callModelWithinBudget(ctx, state, route, runtimeModelCall{
		task:             finalTask,
		prompt:           state.finalPrompt,
		budgetStage:      "final_budget",
		tokenBudgetStage: "final_token_budget",
		modelStage:       "final_model",
		modelError:       ErrFinalModelFailed,
	})
	state.finalResponse = response
	if err != nil {
		return runtimeState{}, err
	}
	return state, nil
}

func (r *Runtime) decodeFinalAnswer(ctx context.Context, state runtimeState) (runtimeState, error) {
	references, err := resolveReferences(state.snapshot, finalPromptName, finalTask, finalTask)
	if err != nil {
		return runtimeState{}, err
	}
	metadata := structuredoutput.ExecutionMetadata{
		Provider:           references.Provider,
		Model:              references.Model,
		PromptVersion:      references.PromptVersion,
		SchemaVersion:      references.SchemaVersion,
		ModelConfigVersion: references.ModelConfigVersion,
	}
	// 最终回答也必须完成独立的结构化解析，确保 answered/refused 等业务状态合法。
	value, report, err := structuredoutput.DecodeFinalAnswer(ctx, []byte(state.finalResponse.Message.Content), structuredoutput.DecodeOptions{
		Metadata: metadata,
		Repair:   r.finalAnswerRepair,
	})
	if err != nil {
		return runtimeState{}, runtimeFailure("final_decode", fmt.Errorf("%w: %w", ErrFinalDecodeFailed, err))
	}
	state.finalAnswer = value
	state.finalAnswerReport = report
	return state, nil
}

func (r *Runtime) formatRunOutput(ctx context.Context, state runtimeState) (RunOutput, error) {
	if err := ctx.Err(); err != nil {
		return RunOutput{}, err
	}
	route, err := routeFromSnapshot(state.snapshot, finalTask)
	if err != nil {
		return RunOutput{}, err
	}
	provider := state.finalResponse.Provider
	if provider == "" {
		provider = route.Provider
	}
	model := state.finalResponse.Model
	if model == "" {
		model = route.Model
	}
	// 这一阶段只负责把中间状态转换成稳定的业务输出，不再发起模型调用。
	return RunOutput{
		RequestID:         state.input.RequestID,
		TenantID:          state.input.TenantID,
		RunID:             state.input.RunID,
		ReleaseVersion:    state.snapshot.ReleaseVersion,
		Provider:          provider,
		Model:             model,
		Analysis:          state.analysis,
		FinalAnswer:       state.finalAnswer,
		AnalysisReport:    state.analysisReport,
		FinalAnswerReport: state.finalAnswerReport,
		Shadow:            state.execution.shadow,
	}, nil
}
