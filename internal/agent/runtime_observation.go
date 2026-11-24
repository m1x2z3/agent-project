package agent

import (
	"context"
	"errors"
	"time"

	"agent-project/internal/configregistry"
	"agent-project/internal/modelgateway"
)

func (r *Runtime) observeExecution(execution *runtimeExecution, output RunOutput, runErr error) {
	if r == nil {
		return
	}
	recordExecutionObservation(r.observer, execution, output, runErr)
}

// recordExecutionObservation 提交一次 Run 的最终脱敏观测。
//
// 普通 Runtime 和 ToolLoopRuntime 都共享 runtimeExecution，因此观测提交
// 也统一放在这里。observed 标记保证即使上层在成功或失败路径重复调用，
// Observer 仍然只收到一次结果。
func recordExecutionObservation(
	observer Observer,
	execution *runtimeExecution,
	output RunOutput,
	runErr error,
) {
	if execution == nil {
		return
	}
	execution.mu.Lock()
	defer execution.mu.Unlock()
	if execution.observed {
		return
	}
	// 观测只提交一次，避免 Chain 返回错误和上层重复处理时产生重复记录。
	execution.observed = true
	execution.observation.Duration = time.Since(execution.started)
	execution.observation.Success = runErr == nil
	execution.observation.StructuredValid = runErr == nil
	if runErr != nil {
		execution.observation.ErrorKind = classifyRuntimeError(runErr)
	}
	if runErr == nil {
		execution.observation.Provider = output.Provider
		execution.observation.Model = output.Model
		execution.observation.ReleaseVersion = output.ReleaseVersion
	}
	if observer == nil {
		observer = noopObserver{}
	}
	observer.Observe(execution.observation)
}

func recordModelResponse(
	execution *runtimeExecution,
	snapshot configregistry.ConfigSnapshot,
	response modelgateway.ChatResponse,
	task string,
) {
	if execution == nil {
		return
	}
	// 该公共函数同时服务普通 Runtime 和 ToolLoopRuntime，统一累加一次模型调用的用量。
	// 一次 Runtime 会调用两次模型，这里统一累加 Token 和成本，
	// 观测层只看到汇总后的 Run 级指标，不接触 Prompt 或模型原文。
	execution.mu.Lock()
	defer execution.mu.Unlock()
	if response.Usage != nil {
		execution.observation.PromptTokens += response.Usage.PromptTokens
		execution.observation.CompletionTokens += response.Usage.CompletionTokens
		execution.observation.TotalTokens += response.Usage.TotalTokens
	}
	route, ok := snapshot.Routes[task]
	if !ok {
		return
	}
	modelName := response.Model
	if modelName == "" {
		modelName = route.Model
	}
	execution.observation.Cost += estimateCost(snapshot, route.Provider, modelName, response.Usage)
}

func recordRoute(execution *runtimeExecution, route configregistry.ModelRouteVersion) {
	if execution == nil {
		return
	}
	execution.mu.Lock()
	defer execution.mu.Unlock()
	execution.observation.Provider = route.Provider
	execution.observation.Model = route.Model
}

func estimateCost(
	snapshot configregistry.ConfigSnapshot,
	provider string,
	model string,
	usage *modelgateway.Usage,
) float64 {
	if usage == nil {
		return 0
	}
	var pricing configregistry.PricingVersion
	var found bool
	for _, candidate := range snapshot.Pricing {
		if candidate.Provider == provider && candidate.Model == model {
			pricing = candidate
			found = true
			break
		}
	}
	if !found {
		return 0
	}
	// 价格来自本次 Run 的配置快照，避免运行中切换配置导致成本计算前后不一致。
	return float64(usage.PromptTokens)/1000*pricing.InputPer1K +
		float64(usage.CompletionTokens)/1000*pricing.OutputPer1K
}

func classifyRuntimeError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrAnalysisModelFailed):
		return "analysis_model_error"
	case errors.Is(err, ErrFinalModelFailed):
		return "final_model_error"
	case errors.Is(err, ErrAnalysisDecodeFailed):
		return "analysis_decode_error"
	case errors.Is(err, ErrToolDecisionModelFailed):
		// 工具决策模型失败与普通分析/最终回答失败分开统计，便于定位循环阶段。
		return "tool_decision_model_error"
	case errors.Is(err, ErrToolDecisionDecodeFailed):
		// 结构化解析失败表示模型已返回，但结果不能进入授权边界。
		return "tool_decision_decode_error"
	case errors.Is(err, ErrToolAuthorizationFailed):
		// 授权失败表示模型意图未获得可信执行上下文。
		return "tool_authorization_error"
	case errors.Is(err, ErrToolExecutionFailed):
		// 工具执行边界或工具自身失败，不能与模型错误混为一类。
		return "tool_execution_error"
	case errors.Is(err, ErrDuplicateToolCall):
		// 重复调用表示模型循环没有产生新的执行意图，必须停止当前循环。
		return "duplicate_tool_call"
	case errors.Is(err, ErrMaxToolCallsExceeded):
		// 达到工具调用上限表示循环被保护性终止。
		return "max_tool_calls_exceeded"
	case errors.Is(err, ErrToolHistoryEncodingFailed):
		// 工具结果无法安全回传模型时，停止循环并记录稳定分类。
		return "tool_history_encoding_error"
	case errors.Is(err, ErrFinalDecodeFailed):
		return "final_decode_error"
	default:
		return "runtime_error"
	}
}

type noopObserver struct{}

func (noopObserver) Observe(RunObservation) {}
