package agent

import (
	"context"
	"fmt"

	"agent-project/internal/configregistry"
	"agent-project/internal/modelgateway"
	"agent-project/internal/recording"
)

// runtimeModelCall 描述一次受控模型调用中因业务阶段而异的参数。
//
// 普通 Runtime 和 ToolLoopRuntime 复用预算、超时、记录和 Token 结算逻辑，
// 各阶段通过这些字段保留原有任务身份、模型输入和错误分类。
type runtimeModelCall struct {
	task             string // 当前模型调用所属任务，同时作为快照路由与观测的任务键。
	prompt           string // 当前阶段已经渲染并允许发送给模型的 Prompt。
	budgetStage      string // 模型调用前预算检查失败时的运行阶段标识。
	tokenBudgetStage string // 模型返回后 Token 结算失败时的运行阶段标识。
	modelStage       string // 模型供应商调用失败时的运行阶段标识。
	modelError       error  // 模型调用失败时需要保留的稳定错误哨兵。
}

// runtimeModelCallContext 保存两种 Runtime 执行模型调用时共享的依赖和状态。
// 它只适配公共调用所需数据，不把普通 Chain 状态或工具循环状态暴露给公共内核。
type runtimeModelCallContext struct {
	model     ModelCaller                   // 供应商无关的模型调用器。
	execution *runtimeExecution             // 当前 Run 独立的预算、观测和持久化执行账本。
	input     RunInput                      // 当前 Run 的脱敏关联标识和业务输入。
	snapshot  configregistry.ConfigSnapshot // 当前 Run 已绑定的不可变配置快照。
}

// invokeModelWithinBudget 执行一次受本次 Run 预算约束的共享模型调用。
//
// 公共流程负责路由观测、模型步数、单次超时、输出 Token 上限、调用记录和
// 用量结算；业务阶段通过 runtimeModelCall 保留各自的 Prompt 和错误分类。
// 即使供应商同时返回响应与错误，也会先记录并结算已经发生的真实用量。
func invokeModelWithinBudget(
	ctx context.Context,
	callContext runtimeModelCallContext,
	route configregistry.ModelRouteVersion,
	call runtimeModelCall,
) (modelgateway.ChatResponse, error) {
	recordRoute(callContext.execution, route)
	if callContext.execution == nil || callContext.execution.budget == nil {
		return modelgateway.ChatResponse{}, ErrRuntimeNotConfigured
	}
	// 模型调用前先检查 Token 余额、Run 总时间和最大步数，被拒绝时不访问供应商。
	if err := callContext.execution.budget.BeginModelStep(recording.ClockFromContext(ctx).Now()); err != nil {
		return modelgateway.ChatResponse{}, runtimeFailure(call.budgetStage, err)
	}
	// 单次模型超时不能超过 Run 剩余时间，避免路由配置绕过总时间预算。
	callTimeout := route.Timeout
	remaining := callContext.execution.budget.RemainingDuration(recording.ClockFromContext(ctx).Now())
	if callTimeout <= 0 || remaining < callTimeout {
		callTimeout = remaining
	}
	// 路由输出上限同时受当前 Run 剩余 Token 约束，取两者中的较小值。
	maxTokens := route.MaxTokens
	remainingTokens := callContext.execution.budget.RemainingTokens()
	if maxTokens <= 0 || remainingTokens < maxTokens {
		maxTokens = remainingTokens
	}

	callStarted := recording.ClockFromContext(ctx).Now()
	response, err := callContext.model.Chat(ctx, modelgateway.ChatRequest{
		Task:        call.task,
		Provider:    route.Provider,
		Model:       route.Model,
		Temperature: float64Pointer(route.Temperature),
		MaxTokens:   maxTokensPointer(maxTokens),
		Timeout:     callTimeout,
		Messages:    []modelgateway.Message{{Role: "user", Content: call.prompt}},
	})
	if callContext.execution != nil && callContext.execution.recorder != nil {
		callContext.execution.recorder.recordModelCall(
			ctx,
			callContext.input,
			route,
			response,
			recording.ClockFromContext(ctx).Now().Sub(callStarted),
			err,
		)
	}
	recordModelResponse(callContext.execution, callContext.snapshot, response, call.task)
	// 即使供应商同时返回错误，只要响应带有真实 Usage，也必须先结算已发生的消耗。
	if response.Usage != nil {
		used := response.Usage.TotalTokens
		if used == 0 {
			used = response.Usage.PromptTokens + response.Usage.CompletionTokens
		}
		if budgetErr := callContext.execution.budget.ChargeTokens(used); budgetErr != nil {
			return modelgateway.ChatResponse{}, runtimeFailure(call.tokenBudgetStage, budgetErr)
		}
	} else {
		// 供应商未返回 Usage 时按实际 Prompt 和响应文本估算，不能将本次调用记为零消耗。
		used := estimateTokenCount(call.prompt) + estimateTokenCount(response.Message.Content)
		if budgetErr := callContext.execution.budget.ChargeEstimatedTokens(used); budgetErr != nil {
			return modelgateway.ChatResponse{}, runtimeFailure(call.tokenBudgetStage, budgetErr)
		}
	}
	if err != nil {
		return modelgateway.ChatResponse{}, runtimeFailure(
			call.modelStage,
			fmt.Errorf("%w: %w", call.modelError, err),
		)
	}
	return response, nil
}

// callModelWithinBudget 从工具循环状态提取公共依赖，复用共享模型调用内核。
func (r *ToolLoopRuntime) callModelWithinBudget(
	ctx context.Context,
	state toolLoopState,
	route configregistry.ModelRouteVersion,
	call runtimeModelCall,
) (modelgateway.ChatResponse, error) {
	return invokeModelWithinBudget(ctx, runtimeModelCallContext{
		model:     r.model,
		execution: state.execution,
		input:     state.input,
		snapshot:  state.snapshot,
	}, route, call)
}

// callModelWithinBudget 从普通 Chain 状态提取公共依赖，复用共享模型调用内核。
func (r *Runtime) callModelWithinBudget(
	ctx context.Context,
	state runtimeState,
	route configregistry.ModelRouteVersion,
	call runtimeModelCall,
) (modelgateway.ChatResponse, error) {
	return invokeModelWithinBudget(ctx, runtimeModelCallContext{
		model:     r.model,
		execution: state.execution,
		input:     state.input,
		snapshot:  state.snapshot,
	}, route, call)
}
