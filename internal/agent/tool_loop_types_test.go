package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-project/internal/configregistry"
	"agent-project/internal/modelgateway"
	"agent-project/internal/rollout"
	"agent-project/internal/structuredoutput"
	"agent-project/internal/tool/registry"
	"agent-project/internal/tool/runner"
	"agent-project/internal/tool/tooling"
)

func TestToolCallRecordStoresOnlyExecutionSummary(t *testing.T) {
	record := ToolCallRecord{
		ToolName:        "search_project_docs",
		ToolVersion:     "v1",
		Status:          tooling.ResultSuccess,
		ArgumentsDigest: "sha256:example",
	}

	if record.ToolName != "search_project_docs" ||
		record.ToolVersion != "v1" ||
		record.Status != tooling.ResultSuccess ||
		record.ArgumentsDigest != "sha256:example" {
		t.Fatalf("ToolCallRecord = %+v", record)
	}
	if record.ErrorCode != "" {
		t.Fatalf("successful record ErrorCode = %q, want empty", record.ErrorCode)
	}
}

func TestToolLoopOutputEmbedsRunOutputAndToolSummary(t *testing.T) {
	output := ToolLoopOutput{
		RunOutput: RunOutput{
			RequestID:      "request-1",
			ReleaseVersion: "release-1",
		},
		ToolCalls: []ToolCallRecord{{
			ToolName:    "get_current_time",
			ToolVersion: "v1",
			Status:      tooling.ResultSuccess,
		}},
		ToolDecisionRounds: 2,
	}

	if output.RequestID != "request-1" ||
		output.ReleaseVersion != "release-1" ||
		len(output.ToolCalls) != 1 ||
		output.ToolCalls[0].ToolName != "get_current_time" ||
		output.ToolDecisionRounds != 2 {
		t.Fatalf("ToolLoopOutput = %+v", output)
	}
}

func TestToolLoopFormatOutputRedactsToolDataAndCountsDecisionRounds(t *testing.T) {
	runtime := &ToolLoopRuntime{}
	state := toolLoopState{
		input: RunInput{
			RequestID: "request-1",
			TenantID:  "tenant-a",
			RunID:     "run-1",
		},
		snapshot: toolLoopSnapshot("v1"),
		analysis: structuredoutput.TaskAnalysis{
			Summary:    "需要查询文档",
			Confidence: 0.9,
		},
		finalAnswer: structuredoutput.FinalAnswer{
			Status: structuredoutput.FinalAnswerStatusAnswered,
			Answer: "已完成",
		},
		finalResponse: modelgateway.ChatResponse{
			Provider: "fake",
			Model:    "fake-final_answer",
		},
		toolHistory: []toolHistoryEntry{{
			Record: ToolCallRecord{
				ToolName:        "search_project_docs",
				ToolVersion:     "v1",
				Status:          tooling.ResultSuccess,
				ArgumentsDigest: "sha256:arguments",
			},
			Data: []byte(`{"sensitive":"must not escape"}`),
		}},
	}

	output, err := runtime.formatToolLoopOutput(context.Background(), state)
	if err != nil {
		t.Fatalf("formatToolLoopOutput() error = %v", err)
	}
	if output.Provider != "fake" || output.Model != "fake-final_answer" ||
		output.ReleaseVersion != "v1" {
		t.Fatalf("output metadata = %+v", output.RunOutput)
	}
	if output.ToolDecisionRounds != 2 || len(output.ToolCalls) != 1 {
		t.Fatalf("tool loop summary = %+v", output)
	}
	if output.ToolCalls[0].ArgumentsDigest != "sha256:arguments" {
		t.Fatalf("tool call summary = %+v", output.ToolCalls[0])
	}
	if strings.Contains(string(output.ToolCalls[0].ArgumentsDigest), "sensitive") {
		t.Fatal("tool result data leaked into output summary")
	}
}

func TestToolLoopFormatOutputCopiesToolCallSummary(t *testing.T) {
	runtime := &ToolLoopRuntime{}
	state := toolLoopState{
		input:    RunInput{RequestID: "request-1", RunID: "run-1"},
		snapshot: toolLoopSnapshot("v1"),
		finalResponse: modelgateway.ChatResponse{
			Provider: "fake",
			Model:    "fake-final_answer",
		},
		toolHistory: []toolHistoryEntry{{
			Record: ToolCallRecord{ToolName: "get_current_time", ToolVersion: "v1"},
		}},
	}

	output, err := runtime.formatToolLoopOutput(context.Background(), state)
	if err != nil {
		t.Fatalf("formatToolLoopOutput() error = %v", err)
	}
	output.ToolCalls[0].ToolName = "changed"
	if state.toolHistory[0].Record.ToolName != "get_current_time" {
		t.Fatal("formatToolLoopOutput() exposed mutable tool history")
	}
}

func TestToolLoopObserveExecutionOnlyOnce(t *testing.T) {
	observer := &recordingObserver{}
	runtime := &ToolLoopRuntime{observer: observer}
	execution := &runtimeExecution{
		started: time.Now(),
		observation: RunObservation{
			RequestID: "request-1",
			RunID:     "run-1",
		},
	}
	output := ToolLoopOutput{
		RunOutput: RunOutput{
			Provider:       "fake",
			Model:          "fake-final_answer",
			ReleaseVersion: "v1",
		},
	}

	runtime.observeToolLoopExecution(execution, output, nil)
	runtime.observeToolLoopExecution(execution, output, nil)
	if len(observer.items) != 1 {
		t.Fatalf("observer items = %d, want 1", len(observer.items))
	}
	if !observer.items[0].Success || observer.items[0].Provider != "fake" {
		t.Fatalf("observation = %+v", observer.items[0])
	}
}

func TestToolLoopConfigStoresExecutionDependencies(t *testing.T) {
	authorizer := ToolCallAuthorizer(func(
		context.Context,
		RunInput,
		structuredoutput.ToolDecision,
	) (runner.ToolCallRequest, error) {
		return runner.ToolCallRequest{}, nil
	})

	config := ToolLoopConfig{
		Authorizer:   authorizer,
		MaxToolCalls: 3,
	}
	if config.Authorizer == nil || config.MaxToolCalls != 3 {
		t.Fatalf("ToolLoopConfig = %+v", config)
	}
}

func TestToolLoopRunExecutesToolThenProducesFinalAnswer(t *testing.T) {
	configStore := configregistry.NewRegistry()
	if err := configStore.Publish(toolLoopSnapshot("v1")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := configStore.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(configStore, rollout.RolloutPolicy{
		CandidateVersion: "v1",
		Percentage:       100,
	})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	tool := &toolLoopTestTool{
		definition: toolLoopExecutionDefinition(),
		result: tooling.ToolResult{
			Status: tooling.ResultSuccess,
			Data:   json.RawMessage(`{"hits":[{"id":"doc-1"}]}`),
		},
	}
	toolRegistry := registry.NewRegistry()
	if err := toolRegistry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	executor, err := runner.NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	model := &toolLoopIntegrationModel{}
	observer := &recordingObserver{}
	runtime, err := NewToolLoopRuntime(ToolLoopConfig{
		Registry: configStore,
		Selector: selector,
		Model:    model,
		Executor: executor,
		Observer: observer,
		Authorizer: func(
			_ context.Context,
			input RunInput,
			decision structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			return runner.ToolCallRequest{
				Decision:    decision,
				ToolVersion: "v1",
				TenantID:    input.TenantID,
				RunID:       input.RunID,
				RequestID:   input.RequestID,
				CapabilityToken: tooling.CapabilityToken{
					TokenID:     "cap-1",
					TenantID:    input.TenantID,
					RunID:       input.RunID,
					ToolName:    decision.ToolName,
					ToolVersion: "v1",
					Permissions: []tooling.Permission{"tool:read"},
					ExpiresAt:   time.Now().Add(time.Hour),
					Nonce:       "nonce-1",
				},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}

	output, err := runtime.Run(context.Background(), RunInput{
		RequestID: "request-integration",
		TenantID:  "tenant-a",
		RunID:     "run-integration",
		Query:     "查询订单服务文档",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if output.ReleaseVersion != "v1" ||
		output.FinalAnswer.Status != structuredoutput.FinalAnswerStatusAnswered ||
		output.FinalAnswer.Answer != "已完成" ||
		len(output.ToolCalls) != 1 ||
		output.ToolCalls[0].ToolName != "tool_loop_test" ||
		output.ToolCalls[0].Status != tooling.ResultSuccess ||
		output.ToolDecisionRounds != 2 {
		t.Fatalf("unexpected output: %+v", output)
	}
	if tool.executeCalls != 1 {
		t.Fatalf("tool execute calls = %d, want 1", tool.executeCalls)
	}
	if len(model.requests) != 4 ||
		model.requests[0].Task != analysisTask ||
		model.requests[1].Task != toolDecisionTask ||
		model.requests[2].Task != toolDecisionTask ||
		model.requests[3].Task != finalTask {
		t.Fatalf("model request tasks = %+v", model.requests)
	}
	if !strings.Contains(model.requests[2].Messages[0].Content, "doc-1") {
		t.Fatalf("next decision prompt omitted tool result: %s", model.requests[2].Messages[0].Content)
	}
	if len(observer.items) != 1 || !observer.items[0].Success ||
		observer.items[0].TotalTokens != 12 {
		t.Fatalf("unexpected observation: %+v", observer.items)
	}
}

func TestToolLoopRunStopsOnContextCancellationBeforeModelCall(t *testing.T) {
	configStore := configregistry.NewRegistry()
	if err := configStore.Publish(toolLoopSnapshot("v1")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := configStore.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(configStore, rollout.RolloutPolicy{
		CandidateVersion: "v1",
		Percentage:       100,
	})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	executor, err := runner.NewExecutor(registry.NewRegistry())
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	model := &toolLoopIntegrationModel{}
	runtime, err := NewToolLoopRuntime(ToolLoopConfig{
		Registry: configStore,
		Selector: selector,
		Model:    model,
		Executor: executor,
		Authorizer: func(
			context.Context,
			RunInput,
			structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			return runner.ToolCallRequest{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = runtime.Run(ctx, RunInput{
		RequestID: "request-canceled",
		TenantID:  "tenant-a",
		RunID:     "run-canceled",
		Query:     "问题",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model requests = %d, want 0", len(model.requests))
	}
}

func TestToolLoopRunObservesModelFailure(t *testing.T) {
	configStore := configregistry.NewRegistry()
	if err := configStore.Publish(toolLoopSnapshot("v1")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := configStore.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(configStore, rollout.RolloutPolicy{
		CandidateVersion: "v1",
		Percentage:       100,
	})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	executor, err := runner.NewExecutor(registry.NewRegistry())
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	expected := errors.New("model unavailable")
	observer := &recordingObserver{}
	runtime, err := NewToolLoopRuntime(ToolLoopConfig{
		Registry: configStore,
		Selector: selector,
		Model:    &toolLoopIntegrationModel{err: expected},
		Executor: executor,
		Observer: observer,
		Authorizer: func(
			context.Context,
			RunInput,
			structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			return runner.ToolCallRequest{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}

	_, err = runtime.Run(context.Background(), RunInput{
		RequestID: "request-model-error",
		TenantID:  "tenant-a",
		RunID:     "run-model-error",
		Query:     "问题",
	})
	if !errors.Is(err, ErrAnalysisModelFailed) || !errors.Is(err, expected) {
		t.Fatalf("Run() error = %v, want analysis model error", err)
	}
	if len(observer.items) != 1 ||
		observer.items[0].Success ||
		observer.items[0].ErrorKind != "analysis_model_error" {
		t.Fatalf("unexpected failure observation: %+v", observer.items)
	}
}

func TestNewToolLoopRuntimeRequiresExecutionDependencies(t *testing.T) {
	if _, err := NewToolLoopRuntime(ToolLoopConfig{}); err != ErrToolLoopNotConfigured {
		t.Fatalf("NewToolLoopRuntime() error = %v, want ErrToolLoopNotConfigured", err)
	}
}

func TestNewToolLoopRuntimeAppliesDefaultMaxToolCalls(t *testing.T) {
	configStore := configregistry.NewRegistry()
	publishRuntimeSnapshot(t, configStore, "v1", "analysis {{.Query}}", "final")
	if err := configStore.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(configStore, rollout.RolloutPolicy{
		CandidateVersion: "v1",
		Percentage:       100,
	})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	executor, err := runner.NewExecutor(registry.NewRegistry())
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	runtime, err := NewToolLoopRuntime(ToolLoopConfig{
		Registry: configStore,
		Selector: selector,
		Model:    &fakeRuntimeModel{},
		Executor: executor,
		Authorizer: func(
			context.Context,
			RunInput,
			structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			return runner.ToolCallRequest{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}
	if runtime.maxCalls != defaultMaxToolCalls {
		t.Fatalf("maxCalls = %d, want %d", runtime.maxCalls, defaultMaxToolCalls)
	}
	if runtime.observer == nil {
		t.Fatal("NewToolLoopRuntime() did not install a default observer")
	}
}

func TestToolLoopSnapshotContainsToolDecisionConfiguration(t *testing.T) {
	snapshot := toolLoopSnapshot("v-tool-loop")

	prompt, ok := snapshot.Prompts[toolDecisionPromptName]
	if !ok || prompt.Name != toolDecisionPromptName || prompt.Version == "" || prompt.Template == "" {
		t.Fatalf("tool decision prompt = %+v, exists=%v", prompt, ok)
	}
	schema, ok := snapshot.Schemas[toolDecisionTask]
	if !ok || schema.Kind != toolDecisionTask || schema.Version != structuredoutput.ToolDecisionSchemaVersion {
		t.Fatalf("tool decision schema = %+v, exists=%v", schema, ok)
	}
	route, ok := snapshot.Routes[toolDecisionTask]
	if !ok || route.Task != toolDecisionTask || route.Provider == "" || route.Model == "" {
		t.Fatalf("tool decision route = %+v, exists=%v", route, ok)
	}
	if _, ok := snapshot.Pricing[route.Model]; !ok {
		t.Fatalf("tool decision pricing for model %q is missing", route.Model)
	}
}

func toolLoopSnapshot(version string) configregistry.ConfigSnapshot {
	snapshot := runtimeSnapshot(version, "analysis {{.Query}}", "final {{.Query}}")
	snapshot.Prompts[toolDecisionPromptName] = configregistry.PromptVersion{
		Name:     toolDecisionPromptName,
		Version:  "prompt.tool_decision.v1",
		Template: "tool decision {{.Query}} {{.Analysis.Summary}} {{.ToolHistory}}",
	}
	snapshot.Schemas[toolDecisionTask] = configregistry.SchemaVersion{
		Kind:    toolDecisionTask,
		Version: structuredoutput.ToolDecisionSchemaVersion,
	}
	snapshot.Routes[toolDecisionTask] = configregistry.ModelRouteVersion{
		Task:     toolDecisionTask,
		Version:  "route.tool_decision.v1",
		Provider: "fake",
		Model:    "fake-tool_decision",
		Timeout:  time.Second,
	}
	snapshot.Pricing["fake-tool_decision"] = configregistry.PricingVersion{
		Provider:    "fake",
		Model:       "fake-tool_decision",
		Version:     "price.v1",
		Currency:    "USD",
		InputPer1K:  1,
		OutputPer1K: 2,
	}
	return snapshot
}

func TestToolLoopSelectAndBindSnapshotUsesSelectedRelease(t *testing.T) {
	configStore := configregistry.NewRegistry()
	for _, version := range []string{"v1", "v2"} {
		if err := configStore.Publish(toolLoopSnapshot(version)); err != nil {
			t.Fatalf("Publish(%s) error = %v", version, err)
		}
	}
	if err := configStore.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(configStore, rollout.RolloutPolicy{
		CandidateVersion: "v2",
		Percentage:       100,
	})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	executor, err := runner.NewExecutor(registry.NewRegistry())
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	runtime, err := NewToolLoopRuntime(ToolLoopConfig{
		Registry: configStore,
		Selector: selector,
		Model:    &fakeRuntimeModel{},
		Executor: executor,
		Authorizer: func(
			context.Context,
			RunInput,
			structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			return runner.ToolCallRequest{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}

	state, err := runtime.selectAndBindSnapshot(context.Background(), RunInput{
		RequestID: "request-tool-loop",
		RunID:     "run-tool-loop",
		Query:     "查询订单错误",
	})
	if err != nil {
		t.Fatalf("selectAndBindSnapshot() error = %v", err)
	}
	if state.snapshot.ReleaseVersion != "v2" {
		t.Fatalf("snapshot release = %q, want v2", state.snapshot.ReleaseVersion)
	}
	bound, err := configStore.GetRunSnapshot("run-tool-loop")
	if err != nil {
		t.Fatalf("GetRunSnapshot() error = %v", err)
	}
	if bound.ReleaseVersion != "v2" {
		t.Fatalf("bound release = %q, want v2", bound.ReleaseVersion)
	}
}

func TestToolLoopSelectAndBindSnapshotStopsWhenContextIsCanceled(t *testing.T) {
	configStore := configregistry.NewRegistry()
	if err := configStore.Publish(toolLoopSnapshot("v1")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := configStore.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(configStore, rollout.RolloutPolicy{
		CandidateVersion: "v1",
		Percentage:       100,
	})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	executor, err := runner.NewExecutor(registry.NewRegistry())
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	runtime, err := NewToolLoopRuntime(ToolLoopConfig{
		Registry: configStore,
		Selector: selector,
		Model:    &fakeRuntimeModel{},
		Executor: executor,
		Authorizer: func(
			context.Context,
			RunInput,
			structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			return runner.ToolCallRequest{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = runtime.selectAndBindSnapshot(ctx, RunInput{
		RequestID: "request-canceled",
		RunID:     "run-canceled",
		Query:     "查询订单错误",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("selectAndBindSnapshot() error = %v, want context.Canceled", err)
	}
	if _, err := configStore.GetRunSnapshot("run-canceled"); !errors.Is(err, configregistry.ErrRunNotFound) {
		t.Fatalf("canceled Run unexpectedly bound: %v", err)
	}
}

func TestToolLoopRunAnalysisReusesBoundSnapshot(t *testing.T) {
	configStore := configregistry.NewRegistry()
	if err := configStore.Publish(toolLoopSnapshot("v1")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := configStore.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(configStore, rollout.RolloutPolicy{
		CandidateVersion: "v1",
		Percentage:       100,
	})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	executor, err := runner.NewExecutor(registry.NewRegistry())
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	model := &fakeRuntimeModel{}
	runtime, err := NewToolLoopRuntime(ToolLoopConfig{
		Registry: configStore,
		Selector: selector,
		Model:    model,
		Executor: executor,
		Authorizer: func(
			context.Context,
			RunInput,
			structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			return runner.ToolCallRequest{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}
	state, err := runtime.selectAndBindSnapshot(context.Background(), RunInput{
		RequestID: "request-analysis",
		RunID:     "run-analysis",
		Query:     "订单服务错误",
	})
	if err != nil {
		t.Fatalf("selectAndBindSnapshot() error = %v", err)
	}
	state, err = runtime.runAnalysis(context.Background(), state)
	if err != nil {
		t.Fatalf("runAnalysis() error = %v", err)
	}
	if state.analysis.Summary != "定位订单服务错误" || state.analysis.Confidence != 0.9 {
		t.Fatalf("analysis = %+v", state.analysis)
	}
	if state.analysisReport.Metadata.SchemaVersion != structuredoutput.TaskAnalysisSchemaVersion {
		t.Fatalf("analysis metadata = %+v", state.analysisReport.Metadata)
	}
	if len(model.requests) != 1 ||
		model.requests[0].Task != analysisTask ||
		model.requests[0].Model != "fake-task_analysis" ||
		!strings.Contains(model.requests[0].Messages[0].Content, "订单服务错误") {
		t.Fatalf("analysis model request = %+v", model.requests)
	}
}

func TestToolLoopCallAndDecodeToolDecision(t *testing.T) {
	configStore := configregistry.NewRegistry()
	if err := configStore.Publish(toolLoopSnapshot("v1")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := configStore.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(configStore, rollout.RolloutPolicy{
		CandidateVersion: "v1",
		Percentage:       100,
	})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	executor, err := runner.NewExecutor(registry.NewRegistry())
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	model := &fakeRuntimeModel{}
	runtime, err := NewToolLoopRuntime(ToolLoopConfig{
		Registry: configStore,
		Selector: selector,
		Model:    model,
		Executor: executor,
		Authorizer: func(
			context.Context,
			RunInput,
			structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			return runner.ToolCallRequest{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}

	state, err := runtime.selectAndBindSnapshot(context.Background(), RunInput{
		RequestID: "request-tool-decision",
		RunID:     "run-tool-decision",
		Query:     "查询订单服务文档",
	})
	if err != nil {
		t.Fatalf("selectAndBindSnapshot() error = %v", err)
	}
	state.analysis = structuredoutput.TaskAnalysis{
		Summary:    "需要查询项目文档",
		Confidence: 0.8,
	}
	state, err = runtime.renderToolDecisionPrompt(context.Background(), state)
	if err != nil {
		t.Fatalf("renderToolDecisionPrompt() error = %v", err)
	}
	state, err = runtime.callToolDecisionModel(context.Background(), state)
	if err != nil {
		t.Fatalf("callToolDecisionModel() error = %v", err)
	}
	state, err = runtime.decodeToolDecision(context.Background(), state)
	if err != nil {
		t.Fatalf("decodeToolDecision() error = %v", err)
	}

	if state.toolDecision.ShouldCall {
		t.Fatalf("tool decision = %+v, want should_call=false", state.toolDecision)
	}
	if state.toolDecisionReport.Metadata.SchemaVersion != structuredoutput.ToolDecisionSchemaVersion ||
		state.toolDecisionReport.Attempts != 1 {
		t.Fatalf("tool decision report = %+v", state.toolDecisionReport)
	}
	if len(model.requests) != 1 ||
		model.requests[0].Task != toolDecisionTask ||
		model.requests[0].Provider != "fake" ||
		model.requests[0].Model != "fake-tool_decision" ||
		!strings.Contains(model.requests[0].Messages[0].Content, "查询订单服务文档") {
		t.Fatalf("tool decision model request = %+v", model.requests)
	}
}

func TestToolLoopCallToolDecisionModelStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (&ToolLoopRuntime{}).callToolDecisionModel(ctx, toolLoopState{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("callToolDecisionModel() error = %v, want context.Canceled", err)
	}
}

func TestToolLoopCallToolDecisionModelWrapsModelError(t *testing.T) {
	expected := errors.New("model unavailable")
	runtime := &ToolLoopRuntime{
		model: &fakeRuntimeModel{err: expected},
	}
	state := toolLoopState{
		snapshot: toolLoopSnapshot("v1"),
	}
	_, err := runtime.callToolDecisionModel(context.Background(), state)
	if !errors.Is(err, ErrToolDecisionModelFailed) || !errors.Is(err, expected) {
		t.Fatalf("callToolDecisionModel() error = %v, want model and stage errors", err)
	}
}

func TestToolLoopDecodeToolDecisionUsesRepairOnce(t *testing.T) {
	configStore := configregistry.NewRegistry()
	if err := configStore.Publish(toolLoopSnapshot("v1")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := configStore.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(configStore, rollout.RolloutPolicy{
		CandidateVersion: "v1",
		Percentage:       100,
	})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	executor, err := runner.NewExecutor(registry.NewRegistry())
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	repairCalls := 0
	runtime, err := NewToolLoopRuntime(ToolLoopConfig{
		Registry: configStore,
		Selector: selector,
		Model:    &fakeRuntimeModel{},
		Executor: executor,
		Authorizer: func(
			context.Context,
			RunInput,
			structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			return runner.ToolCallRequest{}, nil
		},
		ToolDecisionRepair: func(
			context.Context,
			structuredoutput.RepairRequest,
		) ([]byte, error) {
			repairCalls++
			return []byte(`{"should_call":false}`), nil
		},
	})
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}
	state := toolLoopState{
		snapshot: toolLoopSnapshot("v1"),
		toolDecisionResponse: modelgateway.ChatResponse{
			Message: modelgateway.Message{Content: `{"should_call":true}`},
		},
	}
	state, err = runtime.decodeToolDecision(context.Background(), state)
	if err != nil {
		t.Fatalf("decodeToolDecision() error = %v", err)
	}
	if repairCalls != 1 || !state.toolDecisionReport.Repaired {
		t.Fatalf("repairCalls = %d, report = %+v", repairCalls, state.toolDecisionReport)
	}
}

func TestToolLoopAuthorizeToolDecisionBuildsValidatedRequest(t *testing.T) {
	configStore := configregistry.NewRegistry()
	if err := configStore.Publish(toolLoopSnapshot("v1")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := configStore.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(configStore, rollout.RolloutPolicy{
		CandidateVersion: "v1",
		Percentage:       100,
	})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	executor, err := runner.NewExecutor(registry.NewRegistry())
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	authorizerCalls := 0
	runtime, err := NewToolLoopRuntime(ToolLoopConfig{
		Registry: configStore,
		Selector: selector,
		Model:    &fakeRuntimeModel{},
		Executor: executor,
		Authorizer: func(
			_ context.Context,
			input RunInput,
			decision structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			authorizerCalls++
			return runner.ToolCallRequest{
				Decision:      decision,
				ToolVersion:   "v1",
				TenantID:      input.TenantID,
				RunID:         input.RunID,
				RequestID:     input.RequestID,
				ResourceScope: []string{"project:project-a"},
				CapabilityToken: tooling.CapabilityToken{
					TokenID:       "cap-1",
					TenantID:      input.TenantID,
					RunID:         input.RunID,
					ToolName:      decision.ToolName,
					ToolVersion:   "v1",
					ResourceScope: []string{"project:project-a"},
					ExpiresAt:     time.Now().Add(time.Hour),
					Nonce:         "nonce-1",
				},
				IdempotencyKey: "idempotency-1",
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}
	state := toolLoopState{
		input: RunInput{
			RequestID: "request-authorize",
			TenantID:  "tenant-a",
			RunID:     "run-authorize",
		},
		toolDecision: structuredoutput.ToolDecision{
			ShouldCall: true,
			ToolName:   "search_project_docs",
			Arguments:  map[string]any{"project_id": "project-a"},
		},
	}
	state, err = runtime.authorizeToolDecision(context.Background(), state)
	if err != nil {
		t.Fatalf("authorizeToolDecision() error = %v", err)
	}
	if authorizerCalls != 1 {
		t.Fatalf("authorizerCalls = %d, want 1", authorizerCalls)
	}
	if state.toolCallRequest.Decision.ToolName != "search_project_docs" ||
		state.toolCallRequest.ToolVersion != "v1" ||
		state.toolCallRequest.TenantID != "tenant-a" {
		t.Fatalf("authorized request = %+v", state.toolCallRequest)
	}
}

func TestToolLoopAuthorizeToolDecisionSkipsWhenNoToolCall(t *testing.T) {
	called := false
	runtime := &ToolLoopRuntime{
		authorizer: func(
			context.Context,
			RunInput,
			structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			called = true
			return runner.ToolCallRequest{}, nil
		},
	}
	state, err := runtime.authorizeToolDecision(context.Background(), toolLoopState{
		toolDecision: structuredoutput.ToolDecision{ShouldCall: false},
	})
	if err != nil {
		t.Fatalf("authorizeToolDecision() error = %v", err)
	}
	if called {
		t.Fatal("authorizer was called for should_call=false")
	}
	if state.toolCallRequest.ToolVersion != "" {
		t.Fatalf("toolCallRequest = %+v, want zero value", state.toolCallRequest)
	}
}

func TestToolLoopAuthorizeToolDecisionWrapsAuthorizerError(t *testing.T) {
	expected := errors.New("authorization unavailable")
	runtime := &ToolLoopRuntime{
		authorizer: func(
			context.Context,
			RunInput,
			structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			return runner.ToolCallRequest{}, expected
		},
	}
	state := toolLoopState{
		input:        RunInput{RequestID: "request-1", TenantID: "tenant-a", RunID: "run-1"},
		toolDecision: structuredoutput.ToolDecision{ShouldCall: true, ToolName: "get_current_time", Arguments: map[string]any{}},
	}
	_, err := runtime.authorizeToolDecision(context.Background(), state)
	if !errors.Is(err, ErrToolAuthorizationFailed) || !errors.Is(err, expected) {
		t.Fatalf("authorizeToolDecision() error = %v, want authorization and cause errors", err)
	}
}

func TestToolLoopAuthorizeToolDecisionRejectsChangedExecutionContext(t *testing.T) {
	runtime := &ToolLoopRuntime{
		authorizer: func(
			_ context.Context,
			input RunInput,
			decision structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			return runner.ToolCallRequest{
				Decision:    decision,
				ToolVersion: "v1",
				TenantID:    input.TenantID,
				RunID:       input.RunID,
				RequestID:   "other-request",
			}, nil
		},
	}
	state := toolLoopState{
		input: RunInput{
			RequestID: "request-1",
			TenantID:  "tenant-a",
			RunID:     "run-1",
		},
		toolDecision: structuredoutput.ToolDecision{
			ShouldCall: true,
			ToolName:   "get_current_time",
			Arguments:  map[string]any{"timezone": "UTC"},
		},
	}
	_, err := runtime.authorizeToolDecision(context.Background(), state)
	if !errors.Is(err, ErrToolAuthorizationFailed) {
		t.Fatalf("authorizeToolDecision() error = %v, want ErrToolAuthorizationFailed", err)
	}
}

func TestToolLoopAuthorizeToolDecisionUsesDeepCopy(t *testing.T) {
	original := structuredoutput.ToolDecision{
		ShouldCall: true,
		ToolName:   "search_project_docs",
		Arguments:  map[string]any{"project_id": "project-a"},
	}
	runtime := &ToolLoopRuntime{
		authorizer: func(
			_ context.Context,
			input RunInput,
			decision structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			decision.Arguments["project_id"] = "project-b"
			return runner.ToolCallRequest{
				Decision:    decision,
				ToolVersion: "v1",
				TenantID:    input.TenantID,
				RunID:       input.RunID,
				RequestID:   input.RequestID,
			}, nil
		},
	}
	state := toolLoopState{
		input: RunInput{
			RequestID: "request-1",
			TenantID:  "tenant-a",
			RunID:     "run-1",
		},
		toolDecision: original,
	}
	_, err := runtime.authorizeToolDecision(context.Background(), state)
	if !errors.Is(err, ErrToolAuthorizationFailed) {
		t.Fatalf("authorizeToolDecision() error = %v, want ErrToolAuthorizationFailed", err)
	}
	if state.toolDecision.Arguments["project_id"] != "project-a" {
		t.Fatalf("original decision was mutated: %+v", state.toolDecision)
	}
}

func TestToolLoopAuthorizeToolDecisionStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	runtime := &ToolLoopRuntime{
		authorizer: func(
			context.Context,
			RunInput,
			structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			called = true
			return runner.ToolCallRequest{}, nil
		},
	}
	_, err := runtime.authorizeToolDecision(ctx, toolLoopState{
		toolDecision: structuredoutput.ToolDecision{ShouldCall: true, ToolName: "get_current_time", Arguments: map[string]any{}},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("authorizeToolDecision() error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("authorizer was called after context cancellation")
	}
}

func TestToolLoopCheckDuplicateToolCallRejectsSameCall(t *testing.T) {
	decision := structuredoutput.ToolDecision{
		ShouldCall: true,
		ToolName:   "search_project_docs",
		Arguments:  map[string]any{"project_id": "project-a", "limit": 3},
	}
	digest, err := digestToolArguments(decision.Arguments)
	if err != nil {
		t.Fatalf("digestToolArguments() error = %v", err)
	}
	runtime := &ToolLoopRuntime{}
	state := toolLoopState{
		toolDecision: decision,
		toolCallRequest: runner.ToolCallRequest{
			Decision:    decision,
			ToolVersion: "v1",
		},
		toolHistory: []toolHistoryEntry{{
			Record: ToolCallRecord{
				ToolName:        decision.ToolName,
				ToolVersion:     "v1",
				ArgumentsDigest: digest,
			},
		}},
	}
	_, err = runtime.checkDuplicateToolCall(context.Background(), state)
	if !errors.Is(err, ErrDuplicateToolCall) {
		t.Fatalf("checkDuplicateToolCall() error = %v, want ErrDuplicateToolCall", err)
	}
}

func TestToolLoopCheckDuplicateToolCallStoresDigestForNewCall(t *testing.T) {
	decision := structuredoutput.ToolDecision{
		ShouldCall: true,
		ToolName:   "search_project_docs",
		Arguments:  map[string]any{"project_id": "project-a"},
	}
	runtime := &ToolLoopRuntime{}
	state, err := runtime.checkDuplicateToolCall(context.Background(), toolLoopState{
		toolDecision: decision,
		toolCallRequest: runner.ToolCallRequest{
			Decision:    decision,
			ToolVersion: "v1",
		},
	})
	if err != nil {
		t.Fatalf("checkDuplicateToolCall() error = %v", err)
	}
	if state.toolArgumentsDigest == "" {
		t.Fatal("toolArgumentsDigest is empty")
	}
}

func TestToolLoopCheckDuplicateToolCallAllowsDifferentVersionOrArguments(t *testing.T) {
	decision := structuredoutput.ToolDecision{
		ShouldCall: true,
		ToolName:   "search_project_docs",
		Arguments:  map[string]any{"project_id": "project-b"},
	}
	oldDigest, err := digestToolArguments(map[string]any{"project_id": "project-a"})
	if err != nil {
		t.Fatalf("digestToolArguments() error = %v", err)
	}
	runtime := &ToolLoopRuntime{}
	state, err := runtime.checkDuplicateToolCall(context.Background(), toolLoopState{
		toolDecision: decision,
		toolCallRequest: runner.ToolCallRequest{
			Decision:    decision,
			ToolVersion: "v2",
		},
		toolHistory: []toolHistoryEntry{{
			Record: ToolCallRecord{
				ToolName:        decision.ToolName,
				ToolVersion:     "v1",
				ArgumentsDigest: oldDigest,
			},
		}},
	})
	if err != nil {
		t.Fatalf("checkDuplicateToolCall() error = %v", err)
	}
	if state.toolArgumentsDigest == oldDigest {
		t.Fatalf("new call reused old digest: %q", state.toolArgumentsDigest)
	}
}

func TestToolLoopCheckDuplicateToolCallStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (&ToolLoopRuntime{}).checkDuplicateToolCall(ctx, toolLoopState{
		toolDecision: structuredoutput.ToolDecision{
			ShouldCall: true,
			ToolName:   "get_current_time",
			Arguments:  map[string]any{"timezone": "UTC"},
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("checkDuplicateToolCall() error = %v, want context.Canceled", err)
	}
}

func TestToolLoopCheckToolCallLimitAllowsCallBeforeLimit(t *testing.T) {
	runtime := &ToolLoopRuntime{maxCalls: 2}
	state, err := runtime.checkToolCallLimit(context.Background(), toolLoopState{
		toolDecision: structuredoutput.ToolDecision{ShouldCall: true},
		toolHistory: []toolHistoryEntry{{
			Record: ToolCallRecord{ToolName: "previous-tool"},
		}},
	})
	if err != nil {
		t.Fatalf("checkToolCallLimit() error = %v", err)
	}
	if len(state.toolHistory) != 1 {
		t.Fatalf("tool history length = %d, want 1", len(state.toolHistory))
	}
}

func TestToolLoopCheckToolCallLimitRejectsCallAtLimit(t *testing.T) {
	runtime := &ToolLoopRuntime{maxCalls: 2}
	_, err := runtime.checkToolCallLimit(context.Background(), toolLoopState{
		toolDecision: structuredoutput.ToolDecision{ShouldCall: true},
		toolHistory: []toolHistoryEntry{
			{Record: ToolCallRecord{ToolName: "tool-1"}},
			{Record: ToolCallRecord{ToolName: "tool-2"}},
		},
	})
	if !errors.Is(err, ErrMaxToolCallsExceeded) {
		t.Fatalf("checkToolCallLimit() error = %v, want ErrMaxToolCallsExceeded", err)
	}
}

func TestToolLoopCheckToolCallLimitSkipsWhenNoToolCall(t *testing.T) {
	runtime := &ToolLoopRuntime{maxCalls: 1}
	state, err := runtime.checkToolCallLimit(context.Background(), toolLoopState{
		toolDecision: structuredoutput.ToolDecision{ShouldCall: false},
		toolHistory: []toolHistoryEntry{
			{Record: ToolCallRecord{ToolName: "tool-1"}},
		},
	})
	if err != nil {
		t.Fatalf("checkToolCallLimit() error = %v", err)
	}
	if len(state.toolHistory) != 1 {
		t.Fatalf("tool history length = %d, want 1", len(state.toolHistory))
	}
}

func TestToolLoopCheckToolCallLimitStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (&ToolLoopRuntime{maxCalls: 1}).checkToolCallLimit(ctx, toolLoopState{
		toolDecision: structuredoutput.ToolDecision{ShouldCall: true},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("checkToolCallLimit() error = %v, want context.Canceled", err)
	}
}

func TestToolLoopExecuteToolAppendsSuccessfulResultToHistory(t *testing.T) {
	tool := &toolLoopTestTool{
		definition: toolLoopExecutionDefinition(),
		result: tooling.ToolResult{
			Status: tooling.ResultSuccess,
			Data:   json.RawMessage(`{"answer":"found"}`),
			Source: []tooling.SourceRef{{
				Source:   "project_docs",
				Location: "project-a/doc-1",
			}},
		},
	}
	executor := newToolLoopTestExecutor(t, tool)
	runtime := &ToolLoopRuntime{executor: executor}
	request := toolLoopExecutionRequest()
	digest, err := digestToolArguments(request.Decision.Arguments)
	if err != nil {
		t.Fatalf("digestToolArguments() error = %v", err)
	}

	state, err := runtime.executeTool(context.Background(), toolLoopState{
		toolDecision:        request.Decision,
		toolCallRequest:     request,
		toolArgumentsDigest: digest,
	})
	if err != nil {
		t.Fatalf("executeTool() error = %v", err)
	}
	if tool.executeCalls != 1 {
		t.Fatalf("tool execute calls = %d, want 1", tool.executeCalls)
	}
	if len(state.toolHistory) != 1 {
		t.Fatalf("tool history length = %d, want 1", len(state.toolHistory))
	}
	entry := state.toolHistory[0]
	if entry.Record.ToolName != request.Decision.ToolName ||
		entry.Record.ToolVersion != request.ToolVersion ||
		entry.Record.Status != tooling.ResultSuccess ||
		entry.Record.ArgumentsDigest != digest {
		t.Fatalf("tool history record = %+v", entry.Record)
	}
	if string(entry.Data) != `{"answer":"found"}` ||
		len(entry.Source) != 1 ||
		entry.Source[0].Location != "project-a/doc-1" {
		t.Fatalf("tool history result = %+v", entry)
	}
}

func TestToolLoopExecuteToolKeepsBusinessFailureForNextRound(t *testing.T) {
	tool := &toolLoopTestTool{
		definition: toolLoopExecutionDefinition(),
		result: tooling.ToolResult{
			Status: tooling.ResultNotFound,
			Error: &tooling.ToolError{
				Code:      "document_not_found",
				Message:   "document not found",
				Retryable: false,
			},
		},
	}
	runtime := &ToolLoopRuntime{executor: newToolLoopTestExecutor(t, tool)}
	request := toolLoopExecutionRequest()

	state, err := runtime.executeTool(context.Background(), toolLoopState{
		toolDecision:    request.Decision,
		toolCallRequest: request,
	})
	if err != nil {
		t.Fatalf("executeTool() error = %v", err)
	}
	if len(state.toolHistory) != 1 {
		t.Fatalf("tool history length = %d, want 1", len(state.toolHistory))
	}
	if state.toolHistory[0].Record.Status != tooling.ResultNotFound ||
		state.toolHistory[0].Record.ErrorCode != "document_not_found" {
		t.Fatalf("business failure record = %+v", state.toolHistory[0].Record)
	}
}

func TestToolLoopExecuteToolClassifiesExecutionBoundaryError(t *testing.T) {
	runtime := &ToolLoopRuntime{executor: mustToolLoopExecutor(t)}
	request := toolLoopExecutionRequest()
	request.Decision.ToolName = "unknown_tool"
	request.CapabilityToken.ToolName = "unknown_tool"

	state, err := runtime.executeTool(context.Background(), toolLoopState{
		toolDecision:    request.Decision,
		toolCallRequest: request,
	})
	if !errors.Is(err, ErrToolExecutionFailed) || !errors.Is(err, registry.ErrToolNotFound) {
		t.Fatalf("executeTool() error = %v, want execution and not-found errors", err)
	}
	if len(state.toolHistory) != 1 {
		t.Fatalf("tool history length = %d, want 1", len(state.toolHistory))
	}
	record := state.toolHistory[0].Record
	if record.Status != tooling.ResultNotFound || record.ErrorCode != "tool_not_found" {
		t.Fatalf("boundary failure record = %+v", record)
	}
}

func TestToolLoopExecuteToolStopsBeforeExecutorWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tool := &toolLoopTestTool{definition: toolLoopExecutionDefinition()}
	runtime := &ToolLoopRuntime{executor: newToolLoopTestExecutor(t, tool)}

	_, err := runtime.executeTool(ctx, toolLoopState{
		toolDecision:    toolLoopExecutionRequest().Decision,
		toolCallRequest: toolLoopExecutionRequest(),
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("executeTool() error = %v, want context.Canceled", err)
	}
	if tool.executeCalls != 0 {
		t.Fatalf("tool execute calls = %d, want 0", tool.executeCalls)
	}
}

func TestToolLoopExecuteToolStopsAtMaximumWithoutCallingExecutor(t *testing.T) {
	tool := &toolLoopTestTool{definition: toolLoopExecutionDefinition()}
	runtime := &ToolLoopRuntime{
		executor: newToolLoopTestExecutor(t, tool),
		maxCalls: 1,
	}
	request := toolLoopExecutionRequest()

	_, err := runtime.executeTool(context.Background(), toolLoopState{
		toolDecision:    request.Decision,
		toolCallRequest: request,
		toolHistory: []toolHistoryEntry{{
			Record: ToolCallRecord{
				ToolName:    "previous-tool",
				ToolVersion: "v1",
				Status:      tooling.ResultSuccess,
			},
		}},
	})
	if !errors.Is(err, ErrMaxToolCallsExceeded) {
		t.Fatalf("executeTool() error = %v, want ErrMaxToolCallsExceeded", err)
	}
	if tool.executeCalls != 0 {
		t.Fatalf("tool execute calls = %d, want 0", tool.executeCalls)
	}
}

func newToolLoopTestExecutor(t *testing.T, tool tooling.Tool) *runner.Executor {
	t.Helper()
	toolRegistry := registry.NewRegistry()
	if err := toolRegistry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	executor, err := runner.NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	return executor
}

func mustToolLoopExecutor(t *testing.T) *runner.Executor {
	t.Helper()
	return newToolLoopTestExecutor(t, &toolLoopTestTool{
		definition: toolLoopExecutionDefinition(),
	})
}

func toolLoopExecutionRequest() runner.ToolCallRequest {
	return runner.ToolCallRequest{
		Decision: structuredoutput.ToolDecision{
			ShouldCall: true,
			ToolName:   "tool_loop_test",
			Arguments:  map[string]any{"query": "error"},
		},
		ToolVersion: "v1",
		TenantID:    "tenant-a",
		RunID:       "run-1",
		RequestID:   "request-1",
		CapabilityToken: tooling.CapabilityToken{
			TokenID:     "cap-1",
			TenantID:    "tenant-a",
			RunID:       "run-1",
			ToolName:    "tool_loop_test",
			ToolVersion: "v1",
			Permissions: []tooling.Permission{"tool:read"},
			ExpiresAt:   time.Now().Add(time.Hour),
			Nonce:       "nonce-1",
		},
	}
}

func toolLoopExecutionDefinition() tooling.ToolDefinition {
	return tooling.ToolDefinition{
		Name:            "tool_loop_test",
		Version:         "v1",
		Description:     "工具循环测试工具",
		Permission:      "tool:read",
		RiskLevel:       tooling.RiskLow,
		EffectClass:     tooling.EffectRead,
		Timeout:         time.Second,
		ParameterSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string"}},"additionalProperties":false}`),
		TenantScope:     tooling.TenantScopeGlobal,
	}
}

type toolLoopTestTool struct {
	definition   tooling.ToolDefinition
	result       tooling.ToolResult
	executeCalls int
}

func (t *toolLoopTestTool) Definition() tooling.ToolDefinition {
	return t.definition
}

func (t *toolLoopTestTool) Execute(context.Context, tooling.ToolExecutionRequest) (tooling.ToolResult, error) {
	t.executeCalls++
	result := t.result
	result.ToolName = t.definition.Name
	result.ToolVersion = t.definition.Version
	return result, nil
}

type toolLoopIntegrationModel struct {
	requests      []modelgateway.ChatRequest
	decisionCalls int
	err           error
}

func (m *toolLoopIntegrationModel) Chat(
	ctx context.Context,
	request modelgateway.ChatRequest,
) (modelgateway.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return modelgateway.ChatResponse{}, err
	}
	m.requests = append(m.requests, request)
	if m.err != nil {
		return modelgateway.ChatResponse{}, m.err
	}

	content := `{"summary":"需要查询项目文档","confidence":0.9}`
	switch request.Task {
	case toolDecisionTask:
		m.decisionCalls++
		if m.decisionCalls == 1 {
			content = `{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"error"}}`
		} else {
			content = `{"should_call":false}`
		}
	case finalTask:
		content = `{"status":"answered","answer":"已完成"}`
	}
	return modelgateway.ChatResponse{
		Provider: request.Provider,
		Model:    request.Model,
		Message:  modelgateway.Message{Role: "assistant", Content: content},
		Usage:    &modelgateway.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
	}, nil
}

func TestDigestToolArgumentsIsStableAndRedacted(t *testing.T) {
	first, err := digestToolArguments(map[string]any{
		"query": "订单服务错误",
		"limit": 3,
	})
	if err != nil {
		t.Fatalf("digestToolArguments() error = %v", err)
	}
	second, err := digestToolArguments(map[string]any{
		"limit": 3,
		"query": "订单服务错误",
	})
	if err != nil {
		t.Fatalf("digestToolArguments() error = %v", err)
	}
	if first != second {
		t.Fatalf("digest values differ: %q != %q", first, second)
	}
	if !strings.HasPrefix(first, "sha256:") || strings.Contains(first, "订单服务错误") {
		t.Fatalf("digest exposes raw arguments: %q", first)
	}
}

func TestDigestToolArgumentsRejectsUnsupportedValues(t *testing.T) {
	_, err := digestToolArguments(map[string]any{
		"callback": func() {},
	})
	if !errors.Is(err, ErrToolHistoryEncodingFailed) {
		t.Fatalf("digestToolArguments() error = %v, want ErrToolHistoryEncodingFailed", err)
	}
}

func TestToolHistoryEntryStoresStructuredResultAndRedactedRecord(t *testing.T) {
	entry := toolHistoryEntry{
		Record: ToolCallRecord{
			ToolName:        "search_project_docs",
			ToolVersion:     "v1",
			Status:          tooling.ResultSuccess,
			ArgumentsDigest: "sha256:example",
		},
		Data: []byte(`{"hits":[]}`),
		Source: []tooling.SourceRef{{
			Source:   "project_docs",
			Location: "project-a",
		}},
	}
	if entry.Record.ArgumentsDigest != "sha256:example" ||
		string(entry.Data) != `{"hits":[]}` ||
		len(entry.Source) != 1 {
		t.Fatalf("tool history entry = %+v", entry)
	}
}

func TestToolLoopRenderToolDecisionPrompt(t *testing.T) {
	configStore := configregistry.NewRegistry()
	if err := configStore.Publish(toolLoopSnapshot("v1")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := configStore.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(configStore, rollout.RolloutPolicy{
		CandidateVersion: "v1",
		Percentage:       100,
	})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	executor, err := runner.NewExecutor(registry.NewRegistry())
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	runtime, err := NewToolLoopRuntime(ToolLoopConfig{
		Registry: configStore,
		Selector: selector,
		Model:    &fakeRuntimeModel{},
		Executor: executor,
		Authorizer: func(
			context.Context,
			RunInput,
			structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			return runner.ToolCallRequest{}, nil
		},
	})
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}
	state := toolLoopState{
		input:    RunInput{Query: "订单服务错误"},
		snapshot: toolLoopSnapshot("v1"),
		analysis: structuredoutput.TaskAnalysis{
			Summary:    "定位订单服务错误",
			Confidence: 0.9,
		},
		toolHistory: []toolHistoryEntry{{
			Record: ToolCallRecord{
				ToolName:        "search_project_docs",
				ToolVersion:     "v1",
				Status:          tooling.ResultSuccess,
				ArgumentsDigest: "sha256:example",
			},
			Data: []byte(`{"hits":[]}`),
		}},
	}

	state, err = runtime.renderToolDecisionPrompt(context.Background(), state)
	if err != nil {
		t.Fatalf("renderToolDecisionPrompt() error = %v", err)
	}
	for _, expected := range []string{
		"订单服务错误",
		"定位订单服务错误",
		"search_project_docs",
		"sha256:example",
		`"hits":[]`,
	} {
		if !strings.Contains(state.toolDecisionPrompt, expected) {
			t.Fatalf("prompt does not contain %q: %s", expected, state.toolDecisionPrompt)
		}
	}
}

func TestEncodeToolHistoryRejectsInvalidData(t *testing.T) {
	_, err := encodeToolHistory([]toolHistoryEntry{{
		Data: []byte(`{"invalid"`),
	}})
	if !errors.Is(err, ErrToolHistoryEncodingFailed) {
		t.Fatalf("encodeToolHistory() error = %v, want ErrToolHistoryEncodingFailed", err)
	}
}

func TestToolLoopRenderToolDecisionPromptStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (&ToolLoopRuntime{}).renderToolDecisionPrompt(ctx, toolLoopState{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("renderToolDecisionPrompt() error = %v, want context.Canceled", err)
	}
}

func TestToolLoopContinueToolDecisionReturnsToolResultToModel(t *testing.T) {
	model := &fakeRuntimeModel{}
	runtime := &ToolLoopRuntime{model: model}
	state := toolLoopState{
		input: RunInput{
			Query: "查询订单服务文档",
		},
		snapshot: toolLoopSnapshot("v1"),
		analysis: structuredoutput.TaskAnalysis{
			Summary:    "需要查询项目文档",
			Confidence: 0.8,
		},
		toolHistory: []toolHistoryEntry{{
			Record: ToolCallRecord{
				ToolName:        "search_project_docs",
				ToolVersion:     "v1",
				Status:          tooling.ResultSuccess,
				ArgumentsDigest: "sha256:example",
			},
			Data: []byte(`{"items":[{"id":"doc-1","summary":"订单服务错误排查"}]}`),
			Source: []tooling.SourceRef{{
				Source:   "project_docs",
				Location: "project-a/doc-1",
			}},
		}},
	}

	state, err := runtime.continueToolDecision(context.Background(), state)
	if err != nil {
		t.Fatalf("continueToolDecision() error = %v", err)
	}
	if state.toolDecision.ShouldCall {
		t.Fatalf("tool decision = %+v, want should_call=false", state.toolDecision)
	}
	if len(model.requests) != 1 {
		t.Fatalf("model requests = %d, want 1", len(model.requests))
	}
	prompt := model.requests[0].Messages[0].Content
	for _, expected := range []string{
		"查询订单服务文档",
		"订单服务错误排查",
		"project-a/doc-1",
		"sha256:example",
	} {
		if !strings.Contains(prompt, expected) {
			t.Fatalf("next decision prompt does not contain %q: %s", expected, prompt)
		}
	}
}

func TestToolLoopContinueToolDecisionStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	model := &fakeRuntimeModel{}
	runtime := &ToolLoopRuntime{model: model}

	_, err := runtime.continueToolDecision(ctx, toolLoopState{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("continueToolDecision() error = %v, want context.Canceled", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model requests = %d, want 0", len(model.requests))
	}
}

func TestToolLoopRenderFinalPromptUsesAnalysisAndToolHistory(t *testing.T) {
	snapshot := toolLoopSnapshot("v1")
	snapshot.Prompts[finalPromptName] = configregistry.PromptVersion{
		Name:     finalPromptName,
		Version:  "prompt.final.v1",
		Template: "final {{.Query}} {{.Analysis.Summary}} {{.ToolHistory}}",
	}
	state, err := (&ToolLoopRuntime{}).renderToolLoopFinalPrompt(context.Background(), toolLoopState{
		input: RunInput{
			Query: "查询订单服务文档",
		},
		snapshot: snapshot,
		analysis: structuredoutput.TaskAnalysis{
			Summary:    "需要查询项目文档",
			Confidence: 0.8,
		},
		toolHistory: []toolHistoryEntry{{
			Record: ToolCallRecord{
				ToolName:        "search_project_docs",
				ToolVersion:     "v1",
				Status:          tooling.ResultSuccess,
				ArgumentsDigest: "sha256:example",
			},
			Data: []byte(`{"items":[{"id":"doc-1"}]}`),
		}},
	})
	if err != nil {
		t.Fatalf("renderToolLoopFinalPrompt() error = %v", err)
	}
	for _, expected := range []string{
		"查询订单服务文档",
		"需要查询项目文档",
		"search_project_docs",
		`"id":"doc-1"`,
	} {
		if !strings.Contains(state.finalPrompt, expected) {
			t.Fatalf("final prompt does not contain %q: %s", expected, state.finalPrompt)
		}
	}
}

func TestToolLoopRenderFinalPromptStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (&ToolLoopRuntime{}).renderToolLoopFinalPrompt(ctx, toolLoopState{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("renderToolLoopFinalPrompt() error = %v, want context.Canceled", err)
	}
}

func TestToolLoopCallFinalModelUsesSnapshotRoute(t *testing.T) {
	model := &fakeRuntimeModel{}
	runtime := &ToolLoopRuntime{model: model}
	execution := &runtimeExecution{}

	state, err := runtime.callToolLoopFinalModel(context.Background(), toolLoopState{
		snapshot:    toolLoopSnapshot("v1"),
		execution:   execution,
		finalPrompt: "请根据工具结果回答",
	})
	if err != nil {
		t.Fatalf("callToolLoopFinalModel() error = %v", err)
	}
	if state.finalResponse.Model != "fake-final_answer" {
		t.Fatalf("final response model = %q, want fake-final_answer", state.finalResponse.Model)
	}
	if len(model.requests) != 1 {
		t.Fatalf("model requests = %d, want 1", len(model.requests))
	}
	request := model.requests[0]
	if request.Task != finalTask ||
		request.Provider != "fake" ||
		request.Model != "fake-final_answer" ||
		request.Messages[0].Content != "请根据工具结果回答" {
		t.Fatalf("final model request = %+v", request)
	}
}

func TestToolLoopCallFinalModelWrapsModelError(t *testing.T) {
	expected := errors.New("model unavailable")
	runtime := &ToolLoopRuntime{
		model: &fakeRuntimeModel{err: expected},
	}

	_, err := runtime.callToolLoopFinalModel(context.Background(), toolLoopState{
		snapshot:    toolLoopSnapshot("v1"),
		finalPrompt: "最终回答",
	})
	if !errors.Is(err, ErrFinalModelFailed) || !errors.Is(err, expected) {
		t.Fatalf("callToolLoopFinalModel() error = %v, want final model and cause errors", err)
	}
}

func TestToolLoopCallFinalModelStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	model := &fakeRuntimeModel{}
	runtime := &ToolLoopRuntime{model: model}

	_, err := runtime.callToolLoopFinalModel(ctx, toolLoopState{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("callToolLoopFinalModel() error = %v, want context.Canceled", err)
	}
	if len(model.requests) != 0 {
		t.Fatalf("model requests = %d, want 0", len(model.requests))
	}
}

func TestToolLoopDecodeFinalAnswerStoresValidatedResult(t *testing.T) {
	runtime := &ToolLoopRuntime{}
	state, err := runtime.decodeToolLoopFinalAnswer(context.Background(), toolLoopState{
		snapshot: toolLoopSnapshot("v1"),
		finalResponse: modelgateway.ChatResponse{
			Message: modelgateway.Message{
				Content: `{"status":"answered","answer":"已定位订单服务错误"}`,
			},
		},
	})
	if err != nil {
		t.Fatalf("decodeToolLoopFinalAnswer() error = %v", err)
	}
	if state.finalAnswer.Status != structuredoutput.FinalAnswerStatusAnswered ||
		state.finalAnswer.Answer != "已定位订单服务错误" {
		t.Fatalf("final answer = %+v", state.finalAnswer)
	}
	if state.finalAnswerReport.Metadata.SchemaVersion != structuredoutput.FinalAnswerSchemaVersion ||
		state.finalAnswerReport.Attempts != 1 {
		t.Fatalf("final answer report = %+v", state.finalAnswerReport)
	}
}

func TestToolLoopDecodeFinalAnswerUsesRepairOnce(t *testing.T) {
	repairCalls := 0
	runtime := &ToolLoopRuntime{
		finalAnswerRepair: func(
			context.Context,
			structuredoutput.RepairRequest,
		) ([]byte, error) {
			repairCalls++
			return []byte(`{"status":"refused","refusal_reason":"无法确认"}`), nil
		},
	}
	state, err := runtime.decodeToolLoopFinalAnswer(context.Background(), toolLoopState{
		snapshot: toolLoopSnapshot("v1"),
		finalResponse: modelgateway.ChatResponse{
			Message: modelgateway.Message{Content: `{"status":"answered"}`},
		},
	})
	if err != nil {
		t.Fatalf("decodeToolLoopFinalAnswer() error = %v", err)
	}
	if repairCalls != 1 || !state.finalAnswerReport.Repaired {
		t.Fatalf("repairCalls = %d, report = %+v", repairCalls, state.finalAnswerReport)
	}
	if state.finalAnswer.Status != structuredoutput.FinalAnswerStatusRefused {
		t.Fatalf("repaired final answer = %+v", state.finalAnswer)
	}
}

func TestToolLoopDecodeFinalAnswerWrapsValidationError(t *testing.T) {
	runtime := &ToolLoopRuntime{}
	_, err := runtime.decodeToolLoopFinalAnswer(context.Background(), toolLoopState{
		snapshot: toolLoopSnapshot("v1"),
		finalResponse: modelgateway.ChatResponse{
			Message: modelgateway.Message{Content: `{"status":"answered"}`},
		},
	})
	if !errors.Is(err, ErrFinalDecodeFailed) {
		t.Fatalf("decodeToolLoopFinalAnswer() error = %v, want ErrFinalDecodeFailed", err)
	}
}

func TestToolLoopDecodeFinalAnswerStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := (&ToolLoopRuntime{}).decodeToolLoopFinalAnswer(ctx, toolLoopState{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("decodeToolLoopFinalAnswer() error = %v, want context.Canceled", err)
	}
}
