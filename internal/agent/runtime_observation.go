package agent

import (
	"context"
	"errors"
	"time"

	"agent-project/internal/configregistry"
	"agent-project/internal/modelgateway"
)

func (r *Runtime) observeExecution(execution *runtimeExecution, output RunOutput, runErr error) {
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
	r.observer.Observe(execution.observation)
}

func (r *Runtime) recordModelResponse(
	execution *runtimeExecution,
	snapshot configregistry.ConfigSnapshot,
	response modelgateway.ChatResponse,
	task string,
) {
	if execution == nil {
		return
	}
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

func (r *Runtime) recordRoute(execution *runtimeExecution, route configregistry.ModelRouteVersion) {
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
	case errors.Is(err, ErrFinalDecodeFailed):
		return "final_decode_error"
	default:
		return "runtime_error"
	}
}

type noopObserver struct{}

func (noopObserver) Observe(RunObservation) {}
