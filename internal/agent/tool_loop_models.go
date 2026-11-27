package agent

import (
	"context"
	"fmt"

	"agent-project/internal/structuredoutput"
)

// runAnalysis 复用现有 Runtime 的任务分析阶段。
//
// Prompt、Schema 和模型路由都从已经绑定的配置快照读取，避免工具循环
// 使用另一套未版本化的分析配置。模型原文只留在当前阶段状态中，
// 后续只使用通过结构化校验的 TaskAnalysis。
func (r *ToolLoopRuntime) runAnalysis(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	// 快照绑定后才检查 Query，使命中时仍使用本次 Run 已固定的安全版本。
	// 每个 Run 只累计一次 Query 检查，命中时同时累计一次注入拦截。
	injectionReport := inspectPromptInjection(state.input.Query)
	recordSecurityCheck(state.execution)
	if injectionReport.blocked() {
		recordSecurityBlock(state.execution, ErrPromptInjectionDetected)
		return toolLoopState{}, runtimeFailure("tool_loop_analysis_security", ErrPromptInjectionDetected)
	}
	// 读取 Prompt、Schema 和路由必须使用已经绑定的快照，不能回到 Registry 查询 active 版本。
	promptVersion, err := promptFromSnapshot(state.snapshot, analysisPromptName)
	if err != nil {
		return toolLoopState{}, err
	}
	// Query 只通过带版本的可信标签 envelope 进入模板，不再以无标签原文拼接。
	queryEnvelope, err := marshalUserInputEnvelope(state.snapshot.ReleaseVersion, state.input.Query)
	if err != nil {
		return toolLoopState{}, fmt.Errorf("%w: encode user input", ErrPromptRenderFailed)
	}
	rendered, err := renderPrompt(promptVersion.Template, map[string]any{
		"Query": queryEnvelope,
	})
	if err != nil {
		return toolLoopState{}, err
	}
	// 路由参数也从同一快照读取，确保 Provider、Model 和生成参数可追溯。
	route, err := routeFromSnapshot(state.snapshot, analysisTask)
	if err != nil {
		return toolLoopState{}, err
	}
	// 公共模型调用保留分析阶段的预算和模型错误分类，并统一完成记录及 Token 结算。
	response, err := r.callModelWithinBudget(ctx, state, route, runtimeModelCall{
		task:             analysisTask,
		prompt:           rendered,
		budgetStage:      "tool_loop_analysis_budget",
		tokenBudgetStage: "tool_loop_analysis_token_budget",
		modelStage:       "tool_loop_analysis_model",
		modelError:       ErrAnalysisModelFailed,
	})
	// 原始响应仅留在当前阶段状态中，最终输出和观测不会暴露模型原文。
	state.analysisPrompt = rendered
	state.analysisResponse = response
	state.analysisContentTrust = trustUntrustedModel
	if err != nil {
		return toolLoopState{}, err
	}

	references, err := resolveReferences(
		state.snapshot,
		analysisPromptName,
		analysisTask,
		analysisTask,
	)
	if err != nil {
		return toolLoopState{}, err
	}
	// 解析元数据必须来自同一份快照，防止使用错误版本的 Schema 解读模型响应。
	value, report, err := structuredoutput.DecodeTaskAnalysis(
		ctx,
		[]byte(response.Message.Content),
		structuredoutput.DecodeOptions{
			Metadata: structuredoutput.ExecutionMetadata{
				Provider:           references.Provider,
				Model:              references.Model,
				PromptVersion:      references.PromptVersion,
				SchemaVersion:      references.SchemaVersion,
				ModelConfigVersion: references.ModelConfigVersion,
			},
			Repair: r.analysisRepair,
		},
	)
	if err != nil {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_analysis_decode",
			fmt.Errorf("%w: %w", ErrAnalysisDecodeFailed, err),
		)
	}
	// 后续工具决策只使用经过结构化校验的分析结果。
	state.analysis = value
	state.analysisReport = report
	return state, nil
}

// renderGovernedToolLoopPrompt 使用同一模板构建未治理基线和实际治理后 Prompt。
//
// before 和 after 都包含同一固定 system、Query、TaskAnalysis、模板文本及版本化历史 envelope，
// 区别只在于历史是否经过压缩、降级和省略。调用方在渲染成功后提交报告，
// 由当前 Run 的执行账本统一累计到 Observer/RunStore 最终观测。
func (r *ToolLoopRuntime) renderGovernedToolLoopPrompt(
	template string,
	state toolLoopState,
) (string, contextGovernanceReport, error) {
	normalized, err := normalizeContextPolicy(r.contextPolicy)
	if err != nil {
		return "", contextGovernanceReport{}, err
	}
	contextVersion := normalized.contextVersion(state.snapshot.ReleaseVersion)
	// 基线使用完整原始历史和相同 envelope，不用旧数组格式，保证统计口径一致。
	baselineEntries, err := projectToolHistory(state.toolHistory)
	if err != nil {
		return "", contextGovernanceReport{}, err
	}
	baselineHistory, err := marshalToolHistoryEnvelope(
		baselineEntries,
		securityVersion(state.snapshot.ReleaseVersion),
		contextVersion,
		0,
	)
	if err != nil {
		return "", contextGovernanceReport{}, err
	}
	governedHistory, report, err := encodeGovernedToolHistory(
		state.toolHistory,
		normalized,
		state.snapshot.ReleaseVersion,
	)
	if err != nil {
		return "", contextGovernanceReport{}, err
	}

	// before 和 after 共享同一组 Query/Analysis envelope，只有历史治理投影不同。
	queryEnvelope, err := marshalUserInputEnvelope(state.snapshot.ReleaseVersion, state.input.Query)
	if err != nil {
		return "", contextGovernanceReport{}, fmt.Errorf("%w: encode user input", ErrPromptRenderFailed)
	}
	analysisEnvelope, err := marshalModelOutputEnvelope(state.snapshot.ReleaseVersion, state.analysis)
	if err != nil {
		return "", contextGovernanceReport{}, fmt.Errorf("%w: encode model analysis", ErrPromptRenderFailed)
	}
	promptData := map[string]any{
		"Query":       queryEnvelope,
		"Analysis":    analysisEnvelope,
		"ToolHistory": baselineHistory,
	}
	baselinePrompt, err := renderPrompt(template, promptData)
	if err != nil {
		return "", contextGovernanceReport{}, err
	}
	promptData["ToolHistory"] = governedHistory
	rendered, err := renderPrompt(template, promptData)
	if err != nil {
		return "", contextGovernanceReport{}, err
	}
	report.tokensBefore = estimateModelRequestTokens(baselinePrompt)
	report.tokensAfter = estimateModelRequestTokens(rendered)
	return rendered, report, nil
}

// renderToolDecisionPrompt 为当前待执行的工具决策轮次构造模型输入。
//
// 工具历史只通过受控 JSON 进入 Prompt；模型看不到 CapabilityToken、
// 完整模型原文或其他执行上下文。Prompt 模板本身来自本次 Run 的配置快照。
// 首次调用用于准备第一轮决策；工具执行后再次调用则用于准备下一轮决策。
func (r *ToolLoopRuntime) renderToolDecisionPrompt(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	// 工具决策 Prompt 和工具历史都必须来自当前固定快照与受控状态。
	promptVersion, err := promptFromSnapshot(state.snapshot, toolDecisionPromptName)
	if err != nil {
		return toolLoopState{}, err
	}
	// 使用完整历史基线和治理后历史构建同模板 Prompt，本轮只发送治理后的结果。
	rendered, report, err := r.renderGovernedToolLoopPrompt(promptVersion.Template, state)
	if err != nil {
		return toolLoopState{}, err
	}
	// 渲染成功即完成一次治理；后续模型失败时也要保留本次指标。
	recordContextGovernance(state.execution, report)
	// 保存本轮 Prompt，下一步模型调用只读取这个已完成的结果。
	state.toolDecisionPrompt = rendered
	return state, nil
}

// continueToolDecision 将工具结果带入下一轮工具决策。
//
// 工具执行完成后，先把脱敏后的 toolHistory 渲染进 Prompt，再调用模型并
// 严格解析新的 ToolDecision。这个阶段只负责“结果回传和下一次决策”，
// 不执行新工具；后续是否授权和执行仍需经过完整边界。
func (r *ToolLoopRuntime) continueToolDecision(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	// 重新渲染 Prompt，使刚追加的工具结果成为模型下一次决策的输入。
	rendered, err := r.renderToolDecisionPrompt(ctx, state)
	if err != nil {
		return toolLoopState{}, err
	}
	// 使用包含最新工具历史的 Prompt 调用模型，但仍只获取调用意图。
	called, err := r.callToolDecisionModel(ctx, rendered)
	if err != nil {
		return toolLoopState{}, err
	}
	// 新的模型输出必须再次经过 Schema 和业务语义校验，不能直接进入授权。
	return r.decodeToolDecision(ctx, called)
}

// renderToolLoopFinalPrompt 为工具循环的最终回答阶段构造模型输入。
//
// 最终回答只使用原始问题、已校验的任务分析和脱敏后的工具历史；
// Prompt 模板来自本次 Run 的配置快照，避免绕过版本绑定读取全局配置。
func (r *ToolLoopRuntime) renderToolLoopFinalPrompt(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	// 最终回答模板必须来自当前 Run 的固定版本。
	promptVersion, err := promptFromSnapshot(state.snapshot, finalPromptName)
	if err != nil {
		return toolLoopState{}, err
	}
	// 最终回答与工具决策使用相同的治理和 Token 统计口径。
	rendered, report, err := r.renderGovernedToolLoopPrompt(promptVersion.Template, state)
	if err != nil {
		return toolLoopState{}, err
	}
	recordContextGovernance(state.execution, report)
	state.finalPrompt = rendered
	return state, nil
}

// callToolLoopFinalModel 调用最终回答阶段的模型。
//
// 模型路由和生成参数都来自本次 Run 的配置快照；模型响应只保存到中间
// 状态，必须在后续 decodeToolLoopFinalAnswer 中通过结构化校验后才能作为
// 最终业务结果使用。该方法只负责 should_call=false 的最终回答分支，
// 不经过 runner.Executor，也不会执行任何工具。
func (r *ToolLoopRuntime) callToolLoopFinalModel(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	// 最终回答使用固定快照中的路由，不能回退到全局默认模型。
	route, err := routeFromSnapshot(state.snapshot, finalTask)
	if err != nil {
		return toolLoopState{}, err
	}
	// 公共模型调用保留最终回答阶段的错误分类，并统一处理预算、记录和 Token 结算。
	response, err := r.callModelWithinBudget(ctx, state, route, runtimeModelCall{
		task:             finalTask,
		prompt:           state.finalPrompt,
		budgetStage:      "tool_loop_final_budget",
		tokenBudgetStage: "tool_loop_final_token_budget",
		modelStage:       "tool_loop_final_model",
		modelError:       ErrFinalModelFailed,
	})
	state.finalResponse = response
	state.finalContentTrust = trustUntrustedModel
	if err != nil {
		return toolLoopState{}, err
	}
	return state, nil
}

// decodeToolLoopFinalAnswer 严格解析最终回答模型的结构化输出。
//
// Prompt、Schema、模型和模型配置版本都从同一份 Run 快照解析，再交给
// structuredoutput.DecodeFinalAnswer 完成 JSON Schema 与业务语义校验；
// 只有通过校验的结果才会保存为最终答案。
func (r *ToolLoopRuntime) decodeToolLoopFinalAnswer(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	// 解析元数据必须与最终回答使用的 Prompt 和路由来自同一份快照。
	references, err := resolveReferences(
		state.snapshot,
		finalPromptName,
		finalTask,
		finalTask,
	)
	if err != nil {
		return toolLoopState{}, err
	}
	value, report, err := structuredoutput.DecodeFinalAnswer(
		ctx,
		[]byte(state.finalResponse.Message.Content),
		structuredoutput.DecodeOptions{
			Metadata: structuredoutput.ExecutionMetadata{
				Provider:           references.Provider,
				Model:              references.Model,
				PromptVersion:      references.PromptVersion,
				SchemaVersion:      references.SchemaVersion,
				ModelConfigVersion: references.ModelConfigVersion,
			},
			Repair: r.finalAnswerRepair,
		},
	)
	if err != nil {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_final_decode",
			fmt.Errorf("%w: %w", ErrFinalDecodeFailed, err),
		)
	}
	// 只有完整通过 Schema 和业务语义校验的结果才进入最终状态。
	state.finalAnswer = value
	state.finalAnswerReport = report
	return state, nil
}

// callToolDecisionModel 使用当前 Run 快照中的工具决策路由调用模型。
//
// 这一阶段只获取模型的结构化决策文本，不把模型输出直接交给工具执行器；
// 后续还必须经过 DecodeToolDecision、授权回调和 runner.Executor。
func (r *ToolLoopRuntime) callToolDecisionModel(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	if err := ctx.Err(); err != nil {
		return toolLoopState{}, err
	}
	// 工具决策路由来自 Run 快照，不使用全局默认模型，保证版本可追溯。
	route, err := routeFromSnapshot(state.snapshot, toolDecisionTask)
	if err != nil {
		return toolLoopState{}, err
	}
	// 公共模型调用保留工具决策阶段的错误分类，并统一处理预算、记录和 Token 结算。
	response, err := r.callModelWithinBudget(ctx, state, route, runtimeModelCall{
		task:             toolDecisionTask,
		prompt:           state.toolDecisionPrompt,
		budgetStage:      "tool_loop_tool_decision_budget",
		tokenBudgetStage: "tool_loop_tool_decision_token_budget",
		modelStage:       "tool_loop_tool_decision_model",
		modelError:       ErrToolDecisionModelFailed,
	})
	// 原始响应只留在中间状态，下一阶段会执行严格结构化校验。
	state.toolDecisionResponse = response
	state.toolDecisionContentTrust = trustUntrustedModel
	if err != nil {
		return toolLoopState{}, err
	}
	return state, nil
}

// decodeToolDecision 对工具决策模型输出执行 Schema 和业务语义校验。
//
// 解析报告记录本次使用的 Prompt、Schema、模型和模型配置版本；
// 非法输出只能通过注入的 RepairFunc 获得一次修复机会。
func (r *ToolLoopRuntime) decodeToolDecision(
	ctx context.Context,
	state toolLoopState,
) (toolLoopState, error) {
	// 先确认 Prompt、Schema、Route 三种版本元数据都来自同一份快照。
	references, err := resolveReferences(
		state.snapshot,
		toolDecisionPromptName,
		toolDecisionTask,
		toolDecisionTask,
	)
	if err != nil {
		return toolLoopState{}, err
	}
	metadata := structuredoutput.ExecutionMetadata{
		Provider:           references.Provider,
		Model:              references.Model,
		PromptVersion:      references.PromptVersion,
		SchemaVersion:      references.SchemaVersion,
		ModelConfigVersion: references.ModelConfigVersion,
	}
	// DecodeToolDecision 会执行严格 JSON、Schema 和业务语义校验，
	// 必要时只调用一次注入的修复函数。
	value, report, err := structuredoutput.DecodeToolDecision(
		ctx,
		[]byte(state.toolDecisionResponse.Message.Content),
		structuredoutput.DecodeOptions{
			Metadata: metadata,
			Repair:   r.toolDecisionRepair,
		},
	)
	if err != nil {
		return toolLoopState{}, runtimeFailure(
			"tool_loop_tool_decision_decode",
			fmt.Errorf("%w: %w", ErrToolDecisionDecodeFailed, err),
		)
	}
	// 只有通过校验的 ToolDecision 才能进入授权阶段。
	state.toolDecision = value
	state.toolDecisionReport = report
	return state, nil
}
