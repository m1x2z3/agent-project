package agent

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"text/template"

	"agent-project/internal/configregistry"
	"agent-project/internal/modelgateway"
	"agent-project/internal/recording"
	"agent-project/internal/runstore"
)

// runExecutionOptions 保存两种 Runtime 创建执行账本时不同的依赖和运行模式。
// 公共执行准备只读取这些参数，不依赖具体 Runtime，也不改变版本选择职责。
type runExecutionOptions struct {
	budgetResolver BudgetResolver       // 解析可信用户、租户和 Run 级预算；空值使用安全默认预算。
	runStore       runstore.Store       // 可选的 Run 记录存储，由执行记录器旁路写入。
	onStoreError   RunStoreErrorHandler // 接收旁路持久化失败，不改变模型和工具主流程。
	shadow         bool                 // 标记是否为候选版本的影子运行，并同步到脱敏观测。
}

// prepareRunExecution 为普通 Runtime 或工具循环准备独立预算、总超时和执行状态。
//
// 总时间预算从配置选择之前开始计算，避免前置阶段脱离本次 Run 的执行约束。
// 返回的 Context 同时携带总超时取消信号和本次 Run 独立的 runtimeExecution，
// 后续模型、工具、观测与 RunStore 记录都使用同一份执行账本。
func prepareRunExecution(
	ctx context.Context,
	input RunInput,
	options runExecutionOptions,
) (context.Context, context.CancelFunc, *runtimeExecution, error) {
	started := recording.ClockFromContext(ctx).Now()
	budgetSet := BudgetSet{}
	var err error
	if options.budgetResolver != nil {
		budgetSet, err = options.budgetResolver.Resolve(ctx, input)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	budget, err := newBudgetController(budgetSet, started)
	if err != nil {
		return nil, nil, nil, err
	}
	// 只有配置用户级预算时才强制要求可信用户身份，保持无用户预算调用的兼容性。
	if budgetSet.User != (BudgetLimits{}) && strings.TrimSpace(input.UserID) == "" {
		return nil, nil, nil, ErrInvalidRunInput
	}
	// 总超时携带结构化预算原因，失败收口时据此恢复准确的超预算终态。
	budgetCause := BudgetViolation{
		Scope:  budget.effective.DurationSource,
		Reason: "total_time_budget_exceeded",
		Limit:  budget.effective.Limits.MaxDuration.Milliseconds(),
		Used:   budget.effective.Limits.MaxDuration.Milliseconds(),
	}
	ctx, cancel := context.WithTimeoutCause(ctx, budget.effective.Limits.MaxDuration, budgetCause)

	execution := &runtimeExecution{
		started:  started,
		budget:   budget,
		shadow:   options.shadow,
		recorder: &runStoreRecorder{store: options.runStore, onError: options.onStoreError},
		observation: RunObservation{
			RequestID: input.RequestID,
			TenantID:  input.TenantID,
			RunID:     input.RunID,
			Shadow:    options.shadow,
		},
	}
	// 通过 Context 把同一个执行账本交给快照绑定和后续阶段，避免重新创建控制器。
	ctx = context.WithValue(ctx, executionContextKey{}, execution)
	return ctx, cancel, execution, nil
}

// prepareExecution 为工具循环适配共享执行准备流程。
// 工具循环没有影子运行模式，版本选择和 Run 快照绑定仍由后续阶段负责。
func (r *ToolLoopRuntime) prepareExecution(
	ctx context.Context,
	input RunInput,
) (context.Context, context.CancelFunc, *runtimeExecution, error) {
	return prepareRunExecution(ctx, input, runExecutionOptions{
		budgetResolver: r.budgetResolver,
		runStore:       r.runStore,
		onStoreError:   r.onStoreError,
	})
}

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

func validateRunInput(input RunInput) error {
	if strings.TrimSpace(input.RequestID) == "" ||
		strings.TrimSpace(input.RunID) == "" ||
		strings.TrimSpace(input.Query) == "" {
		return ErrInvalidRunInput
	}
	return nil
}

func promptFromSnapshot(snapshot configregistry.ConfigSnapshot, name string) (configregistry.PromptVersion, error) {
	prompt, ok := snapshot.Prompts[name]
	if !ok || strings.TrimSpace(prompt.Template) == "" || strings.TrimSpace(prompt.Version) == "" {
		return configregistry.PromptVersion{}, ErrPromptNotFound
	}
	return prompt, nil
}

func routeFromSnapshot(snapshot configregistry.ConfigSnapshot, task string) (configregistry.ModelRouteVersion, error) {
	// 路由也必须从绑定快照读取，确保任务使用的 Provider、Model 和参数可追溯。
	route, ok := snapshot.Routes[task]
	if !ok || strings.TrimSpace(route.Provider) == "" || strings.TrimSpace(route.Model) == "" {
		return configregistry.ModelRouteVersion{}, ErrRouteNotFound
	}
	return route, nil
}

func schemaFromSnapshot(snapshot configregistry.ConfigSnapshot, kind string) (configregistry.SchemaVersion, error) {
	schema, ok := snapshot.Schemas[kind]
	if !ok || strings.TrimSpace(schema.Kind) == "" || strings.TrimSpace(schema.Version) == "" {
		return configregistry.SchemaVersion{}, ErrSchemaNotFound
	}
	return schema, nil
}

func resolveReferences(
	snapshot configregistry.ConfigSnapshot,
	promptName string,
	schemaKind string,
	task string,
) (configregistry.VersionReferences, error) {
	// 先分别确认 Prompt、Schema、Route 存在，再统一生成版本元数据，
	// 让结构化输出报告能够记录本次解析使用的完整版本组合。
	if _, err := promptFromSnapshot(snapshot, promptName); err != nil {
		return configregistry.VersionReferences{}, err
	}
	if _, err := schemaFromSnapshot(snapshot, schemaKind); err != nil {
		return configregistry.VersionReferences{}, err
	}
	if _, err := routeFromSnapshot(snapshot, task); err != nil {
		return configregistry.VersionReferences{}, err
	}
	return snapshot.ResolveVersionReferences(promptName, schemaKind, task)
}

func renderPrompt(source string, data map[string]any) (string, error) {
	// missingkey=error 会把模板变量拼写错误或配置遗漏立即变成错误，
	// 避免模型收到缺少上下文的半成品 Prompt。
	tmpl, err := template.New("runtime-prompt").Option("missingkey=error").Parse(source)
	if err != nil {
		return "", fmt.Errorf("%w: template parse failed", ErrPromptRenderFailed)
	}
	var output bytes.Buffer
	if err := tmpl.Execute(&output, data); err != nil {
		return "", fmt.Errorf("%w: template execution failed", ErrPromptRenderFailed)
	}
	if strings.TrimSpace(output.String()) == "" {
		return "", fmt.Errorf("%w: rendered prompt is empty", ErrPromptRenderFailed)
	}
	return output.String(), nil
}

func executionFromContext(ctx context.Context) *runtimeExecution {
	execution, _ := ctx.Value(executionContextKey{}).(*runtimeExecution)
	return execution
}

func (r *Runtime) selectorCandidateVersion() string {
	if r == nil || r.selector == nil {
		return ""
	}
	return r.selector.Policy().CandidateVersion
}

func float64Pointer(value float64) *float64 {
	return &value
}

func maxTokensPointer(value int) *int {
	if value <= 0 {
		return nil
	}
	return &value
}
