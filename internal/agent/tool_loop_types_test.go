package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-project/internal/configregistry"
	"agent-project/internal/modelgateway"
	"agent-project/internal/rollout"
	"agent-project/internal/runstore"
	"agent-project/internal/runstore/memory"
	"agent-project/internal/structuredoutput"
	"agent-project/internal/tool/registry"
	"agent-project/internal/tool/runner"
	"agent-project/internal/tool/tooling"
)

// newToolLoopTestExecution 为直接调用工具循环阶段的测试创建默认执行账本。
func newToolLoopTestExecution(t *testing.T) *runtimeExecution {
	t.Helper()
	started := time.Now()
	budget, err := newBudgetController(BudgetSet{}, started)
	if err != nil {
		t.Fatalf("newBudgetController() error = %v", err)
	}
	return &runtimeExecution{started: started, budget: budget}
}

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

	runtime.observeToolLoopExecution(
		context.Background(),
		RunInput{RequestID: "request-1", TenantID: "tenant-1", RunID: "run-1"},
		execution,
		output,
		nil,
	)
	runtime.observeToolLoopExecution(
		context.Background(),
		RunInput{RequestID: "request-1", TenantID: "tenant-1", RunID: "run-1"},
		execution,
		output,
		nil,
	)
	if len(observer.items) != 1 {
		t.Fatalf("observer items = %d, want 1", len(observer.items))
	}
	if !observer.items[0].Success || observer.items[0].Provider != "fake" {
		t.Fatalf("observation = %+v", observer.items[0])
	}
}

func TestToolLoopObserveExecutionFinishesRun(t *testing.T) {
	tests := []struct {
		name       string
		runErr     error
		wantStatus runstore.RunStatus
		wantEvent  runstore.EventType
	}{
		{name: "success", wantStatus: runstore.RunSucceeded, wantEvent: runstore.EventRunSucceeded},
		{name: "failure", runErr: errors.New("model failed"), wantStatus: runstore.RunFailed, wantEvent: runstore.EventRunFailed},
		{name: "canceled", runErr: context.Canceled, wantStatus: runstore.RunCanceled, wantEvent: runstore.EventRunCanceled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &recordingStore{}
			execution := &runtimeExecution{
				started: time.Now(),
				recorder: &runStoreRecorder{
					store: store,
				},
				observation: RunObservation{
					RequestID: "request-finish-" + test.name,
					TenantID:  "tenant-a",
					RunID:     "run-finish-" + test.name,
				},
			}
			input := RunInput{
				RequestID: execution.observation.RequestID,
				TenantID:  execution.observation.TenantID,
				RunID:     execution.observation.RunID,
			}
			runtime := &ToolLoopRuntime{}

			runtime.observeToolLoopExecution(
				context.Background(),
				input,
				execution,
				ToolLoopOutput{},
				test.runErr,
			)

			if store.finishedStatus != test.wantStatus {
				t.Fatalf("finished status = %q, want %q", store.finishedStatus, test.wantStatus)
			}
			if len(store.events) != 1 || store.events[0].Type != test.wantEvent {
				t.Fatalf("finish events = %+v, want %q", store.events, test.wantEvent)
			}
		})
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

func TestNewToolLoopRuntimeStoresOptionalRunStoreDependencies(t *testing.T) {
	registry := configregistry.NewRegistry()
	if err := registry.Publish(toolLoopSnapshot("v1")); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
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

	store := memory.NewMemoryStore()
	handler := func(RunStoreFailure) {}
	runtime, err := NewToolLoopRuntime(ToolLoopConfig{
		Registry: registry,
		Selector: selector,
		Model:    &fakeRuntimeModel{},
		Executor: &runner.Executor{},
		Authorizer: func(context.Context, RunInput, structuredoutput.ToolDecision) (runner.ToolCallRequest, error) {
			return runner.ToolCallRequest{}, nil
		},
		RunStore:     store,
		OnStoreError: handler,
	})
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}
	if runtime.runStore != store {
		t.Fatal("ToolLoopRuntime did not retain RunStore")
	}
	if runtime.onStoreError == nil {
		t.Fatal("ToolLoopRuntime did not retain OnStoreError")
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
			Data:   json.RawMessage(`{"hits":[{"id":"doc-1","secret":"tool-secret"}]}`),
		},
	}
	toolRegistry := registry.NewToolRegistry()
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
	// 工具实例只能收到 Executor 和 tooling.Invoke 校验后的执行请求，
	// 不能直接接收模型原始输出；这里确认可信授权字段已经被注入。
	if tool.lastRequest.TenantID != "tenant-a" ||
		tool.lastRequest.RunID != "run-integration" ||
		tool.lastRequest.RequestID != "request-integration" ||
		tool.lastRequest.CapabilityToken.ToolVersion != "v1" {
		t.Fatalf("unexpected tool execution request: %+v", tool.lastRequest)
	}
	if len(model.requests) != 4 ||
		model.requests[0].Task != analysisTask ||
		model.requests[1].Task != toolDecisionTask ||
		model.requests[2].Task != toolDecisionTask ||
		model.requests[3].Task != finalTask {
		t.Fatalf("model request tasks = %+v", model.requests)
	}
	if !strings.Contains(model.requests[2].Messages[1].Content, "doc-1") {
		t.Fatalf("next decision prompt omitted tool result: %s", model.requests[2].Messages[1].Content)
	}
	// 最终输出只保留工具调用摘要，不应暴露完整参数或工具返回全文。
	outputJSON, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("Marshal(output) error = %v", err)
	}
	if strings.Contains(string(outputJSON), "error") ||
		strings.Contains(string(outputJSON), "tool-secret") {
		t.Fatalf("final output exposed sensitive tool data: %s", outputJSON)
	}
	if len(observer.items) != 1 || !observer.items[0].Success ||
		observer.items[0].TotalTokens != 12 {
		t.Fatalf("unexpected observation: %+v", observer.items)
	}
}

func TestToolLoopRunRecordsStartedAndModelCallEvents(t *testing.T) {
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
			Data:   json.RawMessage(`{"hits":[]}`),
		},
	}
	toolRegistry := registry.NewToolRegistry()
	if err := toolRegistry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	executor, err := runner.NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	store := memory.NewMemoryStore()
	runtime, err := NewToolLoopRuntime(ToolLoopConfig{
		Registry: configStore,
		Selector: selector,
		Model:    &toolLoopIntegrationModel{},
		Executor: executor,
		RunStore: store,
		Authorizer: func(
			_ context.Context,
			input RunInput,
			decision structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			return validToolLoopCallRequest(input, decision), nil
		},
	})
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}

	input := RunInput{
		RequestID: "request-store-model-events",
		TenantID:  "tenant-a",
		RunID:     "run-store-model-events",
		Query:     "查询订单服务文档",
	}
	if _, err := runtime.Run(context.Background(), input); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	events, err := store.ListEvents(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	if len(events) != 8 {
		t.Fatalf("events count = %d, want 8: %+v", len(events), events)
	}
	if events[0].Type != runstore.EventRunStarted {
		t.Fatalf("first event = %+v, want run.started", events[0])
	}
	if events[3].Type != runstore.EventToolCall ||
		events[3].ToolName != "tool_loop_test" ||
		events[3].ToolVersion != "v1" ||
		events[3].Status != "started" ||
		events[3].ArgumentsDigest == "" {
		t.Fatalf("tool call event = %+v", events[3])
	}
	if events[4].Type != runstore.EventToolResult ||
		events[4].ToolName != "tool_loop_test" ||
		events[4].ToolVersion != "v1" ||
		events[4].Status != string(tooling.ResultSuccess) ||
		events[4].DurationMS < 0 ||
		events[4].ArgumentsDigest == "" ||
		events[4].ResultDigest == "" {
		t.Fatalf("tool result event = %+v", events[4])
	}
	for _, event := range events[3:5] {
		eventText := fmt.Sprintf("%+v", event)
		if strings.Contains(eventText, "tool-secret") ||
			strings.Contains(eventText, "error") {
			t.Fatalf("tool event contains sensitive content: %s", eventText)
		}
	}
	wantModels := []string{
		"fake-task_analysis",
		"fake-tool_decision",
		"fake-tool_decision",
		"fake-final_answer",
	}
	for index, wantModel := range wantModels {
		eventIndex := index + 1
		if index >= 2 {
			eventIndex += 2
		}
		event := events[eventIndex]
		if event.Type != runstore.EventModelCall ||
			event.Model != wantModel ||
			event.Provider != "fake" ||
			event.Status != "success" ||
			event.DurationMS < 0 {
			t.Fatalf("model event %d = %+v, want model %q", index, event, wantModel)
		}
		eventText := fmt.Sprintf("%+v", event)
		if strings.Contains(eventText, "查询订单服务文档") ||
			strings.Contains(eventText, "已完成") {
			t.Fatalf("model event contains sensitive content: %s", eventText)
		}
	}
	if events[7].Type != runstore.EventRunSucceeded ||
		events[7].Status != string(runstore.RunSucceeded) {
		t.Fatalf("finish event = %+v, want run.succeeded", events[7])
	}
	record, err := store.GetRun(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if record.Status != runstore.RunSucceeded {
		t.Fatalf("run status = %q, want %q", record.Status, runstore.RunSucceeded)
	}
}

func TestToolLoopRunStoreRecordsToolResolutionFailure(t *testing.T) {
	model := &toolLoopFailureModel{
		decisionContent: `{"should_call":true,"tool_name":"missing_tool","arguments":{"query":"sensitive-query"}}`,
	}
	store := memory.NewMemoryStore()
	input := toolLoopRunInput("run-store-tool-not-found")
	runtime := newToolLoopRunStoreRuntime(
		t,
		model,
		nil,
		store,
		func(
			_ context.Context,
			input RunInput,
			decision structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			return validToolLoopCallRequest(input, decision), nil
		},
	)

	_, err := runtime.Run(context.Background(), input)
	if !errors.Is(err, ErrToolExecutionFailed) ||
		!errors.Is(err, registry.ErrToolNotFound) {
		t.Fatalf("Run() error = %v, want tool execution and not-found errors", err)
	}

	events, err := store.ListEvents(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	wantTypes := []runstore.EventType{
		runstore.EventRunStarted,
		runstore.EventModelCall,
		runstore.EventModelCall,
		runstore.EventRunFailed,
	}
	assertToolLoopEventTypes(t, events, wantTypes)
	// 工具解析属于执行前只读预检，失败时不得伪造 tool.call/tool.result 事实。
	assertToolLoopEventsRedacted(t, events, input.Query, "sensitive-query")

	record, err := store.GetRun(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if record.Status != runstore.RunFailed ||
		record.Config.ReleaseVersion != "v1" ||
		record.Config.PromptVersions["tool_decision"] != "prompt.tool_decision.v1" ||
		record.Config.SchemaVersions["tool_decision"] != structuredoutput.ToolDecisionSchemaVersion ||
		record.Config.RouteVersions["tool_decision"] != "route.tool_decision.v1" {
		t.Fatalf("stored failed run = %+v", record)
	}
}

func TestToolLoopRunStoreRecordsToolExecutionFailure(t *testing.T) {
	model := &toolLoopFailureModel{
		decisionContent: `{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"sensitive-query"}}`,
	}
	tool := &errorToolLoopTestTool{
		definition: toolLoopExecutionDefinition(),
		err:        errors.New("sensitive tool backend failure"),
	}
	store := memory.NewMemoryStore()
	input := toolLoopRunInput("run-store-tool-failure")
	runtime := newToolLoopRunStoreRuntime(
		t,
		model,
		tool,
		store,
		func(
			_ context.Context,
			input RunInput,
			decision structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			return validToolLoopCallRequest(input, decision), nil
		},
	)

	_, err := runtime.Run(context.Background(), input)
	if !errors.Is(err, ErrToolExecutionFailed) {
		t.Fatalf("Run() error = %v, want tool execution failure", err)
	}

	events, err := store.ListEvents(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	wantTypes := []runstore.EventType{
		runstore.EventRunStarted,
		runstore.EventModelCall,
		runstore.EventModelCall,
		runstore.EventToolCall,
		runstore.EventToolResult,
		runstore.EventRunFailed,
	}
	assertToolLoopEventTypes(t, events, wantTypes)
	if events[4].Status != string(tooling.ResultInternalError) ||
		events[4].ErrorKind != "execution_failed" ||
		events[4].ArgumentsDigest == "" ||
		events[4].ResultDigest == "" {
		t.Fatalf("tool execution failure event = %+v", events[4])
	}
	assertToolLoopEventsRedacted(
		t,
		events,
		input.Query,
		"sensitive-query",
		"sensitive tool backend failure",
	)

	record, err := store.GetRun(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if record.Status != runstore.RunFailed ||
		record.Config.ReleaseVersion != "v1" {
		t.Fatalf("stored failed run = %+v", record)
	}
	// 执行器错误优先：保留 Query 和执行前检查，但不增加结果检查或安全拦截。
	if record.Observation.SecurityVersion != "security.v1:v1" ||
		record.Observation.SecurityChecks != 2 || record.Observation.SecurityBlocks != 0 {
		t.Fatalf("tool execution failure security observation = %+v", record.Observation)
	}
}

func TestToolLoopRunStoreRecordsToolCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tool := &blockingToolLoopTestTool{
		definition: toolLoopExecutionDefinition(),
		started:    make(chan struct{}),
	}
	model := &toolLoopSequenceModel{
		decisions: []string{
			`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"sensitive-query"}}`,
		},
	}
	store := memory.NewMemoryStore()
	input := toolLoopRunInput("run-store-tool-canceled")
	runtime := newToolLoopRunStoreRuntime(
		t,
		model,
		tool,
		store,
		func(
			_ context.Context,
			input RunInput,
			decision structuredoutput.ToolDecision,
		) (runner.ToolCallRequest, error) {
			return validToolLoopCallRequest(input, decision), nil
		},
	)

	errCh := make(chan error, 1)
	go func() {
		_, runErr := runtime.Run(ctx, input)
		errCh <- runErr
	}()

	select {
	case <-tool.started:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("tool execution did not start")
	}

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}

	events, err := store.ListEvents(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	wantTypes := []runstore.EventType{
		runstore.EventRunStarted,
		runstore.EventModelCall,
		runstore.EventModelCall,
		runstore.EventToolCall,
		runstore.EventToolResult,
		runstore.EventRunCanceled,
	}
	assertToolLoopEventTypes(t, events, wantTypes)
	if events[4].Status != string(tooling.ResultInternalError) ||
		events[4].ErrorKind != "canceled" ||
		events[4].ArgumentsDigest == "" ||
		events[4].ResultDigest == "" {
		t.Fatalf("tool cancellation event = %+v", events[4])
	}
	assertToolLoopEventsRedacted(t, events, input.Query, "sensitive-query")

	record, err := store.GetRun(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if record.Status != runstore.RunCanceled ||
		record.Config.ReleaseVersion != "v1" {
		t.Fatalf("stored canceled run = %+v", record)
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
	executor, err := runner.NewExecutor(registry.NewToolRegistry())
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

func TestToolLoopRunStopsWhenContextCanceledAfterAnalysis(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	model := &toolLoopCancelAfterAnalysisModel{cancel: cancel}
	runtime := newToolLoopFailureRuntime(t, model, nil, &recordingObserver{}, func(
		context.Context,
		RunInput,
		structuredoutput.ToolDecision,
	) (runner.ToolCallRequest, error) {
		t.Fatal("authorizer was called after context cancellation")
		return runner.ToolCallRequest{}, nil
	})

	_, err := runtime.Run(ctx, toolLoopRunInput("run-cancel-after-analysis"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if len(model.requests) != 1 || model.requests[0].Task != analysisTask {
		t.Fatalf("model requests = %+v, want analysis only", model.requests)
	}
}

func TestToolLoopRunStopsWhenContextCanceledDuringToolExecution(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tool := &blockingToolLoopTestTool{
		definition: toolLoopExecutionDefinition(),
		started:    make(chan struct{}),
	}
	model := &toolLoopSequenceModel{
		decisions: []string{
			`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"error"}}`,
		},
	}
	observer := &recordingObserver{}
	runtime := newToolLoopFailureRuntime(t, model, tool, observer, func(
		_ context.Context,
		input RunInput,
		decision structuredoutput.ToolDecision,
	) (runner.ToolCallRequest, error) {
		return validToolLoopCallRequest(input, decision), nil
	})

	errCh := make(chan error, 1)
	go func() {
		_, runErr := runtime.Run(ctx, toolLoopRunInput("run-cancel-during-tool"))
		errCh <- runErr
	}()

	select {
	case <-tool.started:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("tool execution did not start")
	}

	err := <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if len(observer.items) != 1 || observer.items[0].Success ||
		observer.items[0].ErrorKind != "canceled" {
		t.Fatalf("unexpected cancellation observation: %+v", observer.items)
	}
}

func TestToolLoopRuntimeSupportsConcurrentRuns(t *testing.T) {
	model := &fakeRuntimeModel{}
	observer := &recordingObserver{}
	runtime := newToolLoopFailureRuntime(t, model, nil, observer, func(
		context.Context,
		RunInput,
		structuredoutput.ToolDecision,
	) (runner.ToolCallRequest, error) {
		t.Fatal("authorizer was called for should_call=false")
		return runner.ToolCallRequest{}, nil
	})

	const runCount = 8
	errCh := make(chan error, runCount)
	var waitGroup sync.WaitGroup
	for index := 0; index < runCount; index++ {
		index := index
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			input := toolLoopRunInput(fmt.Sprintf("run-concurrent-%d", index))
			output, err := runtime.Run(context.Background(), input)
			if err != nil {
				errCh <- err
				return
			}
			if output.RunID != input.RunID ||
				output.RequestID != input.RequestID ||
				output.ReleaseVersion != "v1" ||
				output.FinalAnswer.Status != structuredoutput.FinalAnswerStatusAnswered {
				errCh <- fmt.Errorf("unexpected concurrent output: %+v", output)
			}
		}()
	}
	waitGroup.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatal(err)
	}
	if len(observer.items) != runCount {
		t.Fatalf("observation count = %d, want %d", len(observer.items), runCount)
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
	executor, err := runner.NewExecutor(registry.NewToolRegistry())
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

func TestToolLoopRunStopsOnToolDecisionDecodeFailure(t *testing.T) {
	model := &toolLoopFailureModel{
		decisionContent: `{"should_call":true}`,
	}
	observer := &recordingObserver{}
	authorizerCalls := 0
	runtime := newToolLoopFailureRuntime(t, model, nil, observer, func(
		context.Context,
		RunInput,
		structuredoutput.ToolDecision,
	) (runner.ToolCallRequest, error) {
		authorizerCalls++
		return runner.ToolCallRequest{}, nil
	})

	_, err := runtime.Run(context.Background(), toolLoopRunInput("run-decision-decode"))
	if !errors.Is(err, ErrToolDecisionDecodeFailed) {
		t.Fatalf("Run() error = %v, want ErrToolDecisionDecodeFailed", err)
	}
	if authorizerCalls != 0 {
		t.Fatalf("authorizer calls = %d, want 0", authorizerCalls)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want analysis and decision calls", len(model.requests))
	}
	assertSingleToolLoopFailureObservation(t, observer, "tool_decision_decode_error")
}

func TestToolLoopRunStopsOnAuthorizationFailure(t *testing.T) {
	expected := errors.New("authorization unavailable")
	model := &toolLoopFailureModel{
		decisionContent: `{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"error"}}`,
	}
	observer := &recordingObserver{}
	authorizerCalls := 0
	runtime := newToolLoopFailureRuntime(t, model, mustToolLoopTestTool(t), observer, func(
		context.Context,
		RunInput,
		structuredoutput.ToolDecision,
	) (runner.ToolCallRequest, error) {
		authorizerCalls++
		return runner.ToolCallRequest{}, expected
	})

	_, err := runtime.Run(context.Background(), toolLoopRunInput("run-authorize"))
	if !errors.Is(err, ErrToolAuthorizationFailed) || !errors.Is(err, expected) {
		t.Fatalf("Run() error = %v, want authorization error and cause", err)
	}
	if authorizerCalls != 1 {
		t.Fatalf("authorizer calls = %d, want 1", authorizerCalls)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want analysis and decision calls", len(model.requests))
	}
	assertSingleToolLoopFailureObservation(t, observer, "tool_authorization_error")
}

func TestToolLoopRunStopsOnToolResolutionFailure(t *testing.T) {
	model := &toolLoopFailureModel{
		decisionContent: `{"should_call":true,"tool_name":"missing_tool","arguments":{"query":"error"}}`,
	}
	observer := &recordingObserver{}
	toolCalls := 0
	runtime := newToolLoopFailureRuntime(t, model, nil, observer, func(
		_ context.Context,
		input RunInput,
		decision structuredoutput.ToolDecision,
	) (runner.ToolCallRequest, error) {
		return validToolLoopCallRequest(input, decision), nil
	})

	_, err := runtime.Run(context.Background(), toolLoopRunInput("run-tool-not-found"))
	if !errors.Is(err, ErrToolExecutionFailed) || !errors.Is(err, registry.ErrToolNotFound) {
		t.Fatalf("Run() error = %v, want tool execution and not-found errors", err)
	}
	if toolCalls != 0 {
		t.Fatalf("tool calls = %d, want 0", toolCalls)
	}
	if len(model.requests) != 2 {
		t.Fatalf("model requests = %d, want analysis and decision calls", len(model.requests))
	}
	assertSingleToolLoopFailureObservation(t, observer, "tool_execution_error")
}

func TestToolLoopRunStopsOnFinalAnswerDecodeFailure(t *testing.T) {
	model := &toolLoopFailureModel{
		decisionContent: `{"should_call":false}`,
		finalContent:    `{"status":"answered"}`,
	}
	observer := &recordingObserver{}
	runtime := newToolLoopFailureRuntime(t, model, nil, observer, func(
		context.Context,
		RunInput,
		structuredoutput.ToolDecision,
	) (runner.ToolCallRequest, error) {
		return runner.ToolCallRequest{}, nil
	})

	_, err := runtime.Run(context.Background(), toolLoopRunInput("run-final-decode"))
	if !errors.Is(err, ErrFinalDecodeFailed) {
		t.Fatalf("Run() error = %v, want ErrFinalDecodeFailed", err)
	}
	if len(model.requests) != 3 {
		t.Fatalf("model requests = %d, want analysis, decision and final calls", len(model.requests))
	}
	assertSingleToolLoopFailureObservation(t, observer, "final_decode_error")
}

func TestToolLoopRunSkipsToolWhenDecisionDoesNotCall(t *testing.T) {
	model := &toolLoopSequenceModel{
		decisions: []string{`{"should_call":false}`},
	}
	tool := &toolLoopTestTool{
		definition: toolLoopExecutionDefinition(),
		result: tooling.ToolResult{
			Status: tooling.ResultSuccess,
			Data:   json.RawMessage(`{"hits":[]}`),
		},
	}
	observer := &recordingObserver{}
	authorizerCalls := 0
	runtime := newToolLoopFailureRuntime(t, model, tool, observer, func(
		context.Context,
		RunInput,
		structuredoutput.ToolDecision,
	) (runner.ToolCallRequest, error) {
		authorizerCalls++
		return runner.ToolCallRequest{}, nil
	})

	output, err := runtime.Run(context.Background(), toolLoopRunInput("run-no-tool"))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if tool.executeCalls != 0 {
		t.Fatalf("tool execute calls = %d, want 0", tool.executeCalls)
	}
	if authorizerCalls != 0 {
		t.Fatalf("authorizer calls = %d, want 0", authorizerCalls)
	}
	if len(output.ToolCalls) != 0 || output.ToolDecisionRounds != 1 {
		t.Fatalf("output tool state = %+v", output)
	}
	if len(observer.items) != 1 || !observer.items[0].Success {
		t.Fatalf("unexpected success observation: %+v", observer.items)
	}
	if len(model.requests) != 3 ||
		model.requests[0].Task != analysisTask ||
		model.requests[1].Task != toolDecisionTask ||
		model.requests[2].Task != finalTask {
		t.Fatalf("model request tasks = %+v", model.requests)
	}
}

func TestToolLoopRunRejectsDuplicateToolCall(t *testing.T) {
	model := &toolLoopSequenceModel{
		decisions: []string{
			`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"error"}}`,
			`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"error"}}`,
		},
	}
	tool := &toolLoopTestTool{
		definition: toolLoopExecutionDefinition(),
		result: tooling.ToolResult{
			Status: tooling.ResultSuccess,
			Data:   json.RawMessage(`{"hits":[]}`),
		},
	}
	observer := &recordingObserver{}
	runtime := newToolLoopFailureRuntime(t, model, tool, observer, func(
		_ context.Context,
		input RunInput,
		decision structuredoutput.ToolDecision,
	) (runner.ToolCallRequest, error) {
		return validToolLoopCallRequest(input, decision), nil
	})

	_, err := runtime.Run(context.Background(), toolLoopRunInput("run-duplicate"))
	if !errors.Is(err, ErrDuplicateToolCall) {
		t.Fatalf("Run() error = %v, want ErrDuplicateToolCall", err)
	}
	if tool.executeCalls != 1 {
		t.Fatalf("tool execute calls = %d, want 1", tool.executeCalls)
	}
	if len(model.requests) != 3 {
		t.Fatalf("model requests = %d, want analysis and two decision calls", len(model.requests))
	}
	assertSingleToolLoopFailureObservation(t, observer, "duplicate_tool_call")
}

func TestToolLoopRunStopsAtMaximumToolCalls(t *testing.T) {
	model := &toolLoopSequenceModel{
		decisions: []string{
			`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"first"}}`,
			`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"second"}}`,
		},
	}
	tool := &toolLoopTestTool{
		definition: toolLoopExecutionDefinition(),
		result: tooling.ToolResult{
			Status: tooling.ResultSuccess,
			Data:   json.RawMessage(`{"hits":[]}`),
		},
	}
	observer := &recordingObserver{}
	runtime := newToolLoopFailureRuntime(t, model, tool, observer, func(
		_ context.Context,
		input RunInput,
		decision structuredoutput.ToolDecision,
	) (runner.ToolCallRequest, error) {
		return validToolLoopCallRequest(input, decision), nil
	})
	runtime.budgetResolver = FixedBudgetResolver{Budgets: BudgetSet{
		Run: BudgetLimits{MaxToolCalls: 1},
	}}

	_, err := runtime.Run(context.Background(), toolLoopRunInput("run-max-calls"))
	if !errors.Is(err, ErrMaxToolCallsExceeded) {
		t.Fatalf("Run() error = %v, want ErrMaxToolCallsExceeded", err)
	}
	if tool.executeCalls != 1 {
		t.Fatalf("tool execute calls = %d, want 1", tool.executeCalls)
	}
	if len(model.requests) != 3 {
		t.Fatalf("model requests = %d, want analysis and two decision calls", len(model.requests))
	}
	assertSingleToolLoopFailureObservation(t, observer, "max_tool_calls_exceeded")
}

func TestToolLoopRedactsSensitiveDataFromOutputAndObservation(t *testing.T) {
	const (
		apiKey        = "sk-test-sensitive-key"
		promptSecret  = "prompt-secret-content"
		modelSecret   = "model-secret-content"
		toolSecret    = "tool-secret-content"
		argumentValue = "argument-secret-content"
	)
	model := &toolLoopSequenceModel{
		decisions: []string{
			`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"` + argumentValue + `"}}`,
			`{"should_call":false}`,
		},
	}
	tool := &toolLoopTestTool{
		definition: toolLoopExecutionDefinition(),
		result: tooling.ToolResult{
			Status: tooling.ResultSuccess,
			Data:   json.RawMessage(`{"answer":"` + toolSecret + `"}`),
		},
	}
	observer := &recordingObserver{}
	runtime := newToolLoopFailureRuntime(t, model, tool, observer, func(
		_ context.Context,
		input RunInput,
		decision structuredoutput.ToolDecision,
	) (runner.ToolCallRequest, error) {
		return validToolLoopCallRequest(input, decision), nil
	})

	output, err := runtime.Run(context.Background(), RunInput{
		RequestID: "request-redaction",
		TenantID:  "tenant-a",
		RunID:     "run-redaction",
		Query:     promptSecret,
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	outputText, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("Marshal(output) error = %v", err)
	}
	observerText := fmt.Sprintf("%+v", observer.items)
	for _, sensitive := range []string{apiKey, promptSecret, modelSecret, toolSecret, argumentValue} {
		if strings.Contains(string(outputText), sensitive) {
			t.Fatalf("output exposed sensitive value %q: %s", sensitive, outputText)
		}
		if strings.Contains(observerText, sensitive) {
			t.Fatalf("observation exposed sensitive value %q: %s", sensitive, observerText)
		}
	}
	if len(output.ToolCalls) != 1 ||
		!strings.HasPrefix(output.ToolCalls[0].ArgumentsDigest, "sha256:") ||
		strings.Contains(output.ToolCalls[0].ArgumentsDigest, argumentValue) {
		t.Fatalf("tool call summary is not redacted: %+v", output.ToolCalls)
	}

	failureObserver := &recordingObserver{}
	failureModel := &toolLoopFailureModel{
		err: errors.New("provider error api_key=" + apiKey + " prompt=" + promptSecret),
	}
	failureRuntime := newToolLoopFailureRuntime(
		t,
		failureModel,
		nil,
		failureObserver,
		func(context.Context, RunInput, structuredoutput.ToolDecision) (runner.ToolCallRequest, error) {
			return runner.ToolCallRequest{}, nil
		},
	)
	_, err = failureRuntime.Run(context.Background(), toolLoopRunInput("run-redaction-error"))
	if err == nil {
		t.Fatal("Run() error = nil, want model failure")
	}
	if strings.Contains(err.Error(), apiKey) || strings.Contains(err.Error(), promptSecret) {
		t.Fatalf("error exposed sensitive model details: %v", err)
	}
	if len(failureObserver.items) != 1 ||
		strings.Contains(fmt.Sprintf("%+v", failureObserver.items[0]), apiKey) ||
		strings.Contains(fmt.Sprintf("%+v", failureObserver.items[0]), promptSecret) {
		t.Fatalf("failure observation exposed sensitive data: %+v", failureObserver.items)
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
	executor, err := runner.NewExecutor(registry.NewToolRegistry())
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
		Template: "tool decision {{.Query}} {{.Analysis}} {{.ToolHistory}}",
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
	executor, err := runner.NewExecutor(registry.NewToolRegistry())
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
	if state.execution.securityVersion != "security.v1:v2" {
		t.Fatalf("security version = %q, want security.v1:v2", state.execution.securityVersion)
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
	executor, err := runner.NewExecutor(registry.NewToolRegistry())
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
	executor, err := runner.NewExecutor(registry.NewToolRegistry())
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
		!strings.Contains(model.requests[0].Messages[1].Content, "订单服务错误") {
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
	executor, err := runner.NewExecutor(registry.NewToolRegistry())
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
		!strings.Contains(model.requests[0].Messages[1].Content, "查询订单服务文档") {
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
		snapshot:  toolLoopSnapshot("v1"),
		execution: newToolLoopTestExecution(t),
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
	executor, err := runner.NewExecutor(registry.NewToolRegistry())
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
	execution := newToolLoopTestExecution(t)
	state := toolLoopState{
		snapshot:  toolLoopSnapshot("v1"),
		execution: execution,
		toolDecisionResponse: modelgateway.ChatResponse{
			Message: modelgateway.Message{Content: `{"should_call":true}`},
		},
	}
	ctx := context.WithValue(context.Background(), executionContextKey{}, execution)
	state, err = runtime.decodeToolDecision(ctx, state)
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
	executor, err := runner.NewExecutor(registry.NewToolRegistry())
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
		execution:           newToolLoopTestExecution(t),
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
		execution:       newToolLoopTestExecution(t),
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
		execution:       newToolLoopTestExecution(t),
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
	}
	request := toolLoopExecutionRequest()
	execution := newToolLoopTestExecution(t)
	budget, err := newBudgetController(BudgetSet{
		Run: BudgetLimits{MaxToolCalls: 1},
	}, time.Now())
	if err != nil {
		t.Fatalf("newBudgetController() error = %v", err)
	}
	execution.budget = budget
	// 先结算一次历史工具调用，使下一次执行在统一控制器中达到次数上限。
	if err := budget.BeginToolCall(time.Now()); err != nil {
		t.Fatalf("BeginToolCall() error = %v", err)
	}

	_, err = runtime.executeTool(context.Background(), toolLoopState{
		toolDecision:    request.Decision,
		toolCallRequest: request,
		execution:       execution,
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
	toolRegistry := registry.NewToolRegistry()
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
	lastRequest  tooling.ToolExecutionRequest
}

func (t *toolLoopTestTool) Definition() tooling.ToolDefinition {
	return t.definition
}

func (t *toolLoopTestTool) Execute(
	_ context.Context,
	request tooling.ToolExecutionRequest,
) (tooling.ToolResult, error) {
	t.executeCalls++
	t.lastRequest = request
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

type toolLoopFailureModel struct {
	requests        []modelgateway.ChatRequest
	decisionContent string
	finalContent    string
	err             error
}

func (m *toolLoopFailureModel) Chat(
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
		content = m.decisionContent
	case finalTask:
		content = m.finalContent
		if content == "" {
			content = `{"status":"answered","answer":"已完成"}`
		}
	}
	return modelgateway.ChatResponse{
		Provider: request.Provider,
		Model:    request.Model,
		Message:  modelgateway.Message{Role: "assistant", Content: content},
	}, nil
}

type toolLoopSequenceModel struct {
	requests  []modelgateway.ChatRequest
	decisions []string
	index     int
}

func (m *toolLoopSequenceModel) Chat(
	ctx context.Context,
	request modelgateway.ChatRequest,
) (modelgateway.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return modelgateway.ChatResponse{}, err
	}
	m.requests = append(m.requests, request)
	content := `{"summary":"需要查询项目文档","confidence":0.9}`
	switch request.Task {
	case toolDecisionTask:
		if m.index < len(m.decisions) {
			content = m.decisions[m.index]
			m.index++
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
	}, nil
}

type toolLoopCancelAfterAnalysisModel struct {
	cancel   context.CancelFunc
	requests []modelgateway.ChatRequest
}

func (m *toolLoopCancelAfterAnalysisModel) Chat(
	ctx context.Context,
	request modelgateway.ChatRequest,
) (modelgateway.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return modelgateway.ChatResponse{}, err
	}
	m.requests = append(m.requests, request)
	if request.Task == analysisTask {
		m.cancel()
	}
	return modelgateway.ChatResponse{
		Provider: request.Provider,
		Model:    request.Model,
		Message: modelgateway.Message{
			Role:    "assistant",
			Content: `{"summary":"需要查询项目文档","confidence":0.9}`,
		},
	}, nil
}

type blockingToolLoopTestTool struct {
	definition tooling.ToolDefinition
	started    chan struct{}
}

type errorToolLoopTestTool struct {
	definition tooling.ToolDefinition
	result     tooling.ToolResult
	err        error
}

func (t *errorToolLoopTestTool) Definition() tooling.ToolDefinition {
	return t.definition
}

func (t *errorToolLoopTestTool) Execute(
	_ context.Context,
	_ tooling.ToolExecutionRequest,
) (tooling.ToolResult, error) {
	return t.result, t.err
}

func (t *blockingToolLoopTestTool) Definition() tooling.ToolDefinition {
	return t.definition
}

func (t *blockingToolLoopTestTool) Execute(
	ctx context.Context,
	_ tooling.ToolExecutionRequest,
) (tooling.ToolResult, error) {
	close(t.started)
	<-ctx.Done()
	return tooling.ToolResult{}, ctx.Err()
}

func newToolLoopFailureRuntime(
	t *testing.T,
	model ModelCaller,
	tool tooling.Tool,
	observer *recordingObserver,
	authorizer ToolCallAuthorizer,
) *ToolLoopRuntime {
	t.Helper()
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
	toolRegistry := registry.NewToolRegistry()
	if tool != nil {
		if err := toolRegistry.Register(tool); err != nil {
			t.Fatalf("Register() error = %v", err)
		}
	}
	executor, err := runner.NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	runtime, err := NewToolLoopRuntime(ToolLoopConfig{
		Registry:   configStore,
		Selector:   selector,
		Model:      model,
		Executor:   executor,
		Authorizer: authorizer,
		Observer:   observer,
	})
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}
	return runtime
}

func newToolLoopRunStoreRuntime(
	t *testing.T,
	model ModelCaller,
	tool tooling.Tool,
	store runstore.Store,
	authorizer ToolCallAuthorizer,
	analysisRepairs ...structuredoutput.RepairFunc,
) *ToolLoopRuntime {
	t.Helper()
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
	toolRegistry := registry.NewToolRegistry()
	if tool != nil {
		if err := toolRegistry.Register(tool); err != nil {
			t.Fatalf("Register() error = %v", err)
		}
	}
	executor, err := runner.NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	var analysisRepair structuredoutput.RepairFunc
	if len(analysisRepairs) > 0 {
		analysisRepair = analysisRepairs[0]
	}
	runtime, err := NewToolLoopRuntime(ToolLoopConfig{
		Registry:       configStore,
		Selector:       selector,
		Model:          model,
		Executor:       executor,
		Authorizer:     authorizer,
		RunStore:       store,
		AnalysisRepair: analysisRepair,
	})
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}
	return runtime
}

func toolLoopRunInput(runID string) RunInput {
	return RunInput{
		RequestID: "request-" + runID,
		TenantID:  "tenant-a",
		RunID:     runID,
		Query:     "查询订单服务文档",
	}
}

func assertToolLoopEventTypes(
	t *testing.T,
	events []runstore.RunEvent,
	wantTypes []runstore.EventType,
) {
	t.Helper()
	if len(events) != len(wantTypes) {
		t.Fatalf("events count = %d, want %d: %+v", len(events), len(wantTypes), events)
	}
	for index, wantType := range wantTypes {
		if events[index].Type != wantType {
			t.Fatalf("events[%d].Type = %q, want %q; events = %+v",
				index, events[index].Type, wantType, events)
		}
	}
}

func assertToolLoopEventsRedacted(
	t *testing.T,
	events []runstore.RunEvent,
	needles ...string,
) {
	t.Helper()
	for _, event := range events {
		eventText := fmt.Sprintf("%+v", event)
		for _, needle := range needles {
			if strings.Contains(eventText, needle) {
				t.Fatalf("event contains sensitive content %q: %s", needle, eventText)
			}
		}
	}
}

func validToolLoopCallRequest(
	input RunInput,
	decision structuredoutput.ToolDecision,
) runner.ToolCallRequest {
	return runner.ToolCallRequest{
		Decision:    decision,
		ToolVersion: "v1",
		TenantID:    input.TenantID,
		RunID:       input.RunID,
		RequestID:   input.RequestID,
		CapabilityToken: tooling.CapabilityToken{
			TokenID:     "cap-" + input.RunID,
			TenantID:    input.TenantID,
			RunID:       input.RunID,
			ToolName:    decision.ToolName,
			ToolVersion: "v1",
			Permissions: []tooling.Permission{"tool:read"},
			ExpiresAt:   time.Now().Add(time.Hour),
			Nonce:       "nonce-" + input.RunID,
		},
	}
}

func mustToolLoopTestTool(t *testing.T) tooling.Tool {
	t.Helper()
	return &toolLoopTestTool{
		definition: toolLoopExecutionDefinition(),
		result: tooling.ToolResult{
			Status: tooling.ResultSuccess,
			Data:   json.RawMessage(`{"hits":[]}`),
		},
	}
}

func assertSingleToolLoopFailureObservation(
	t *testing.T,
	observer *recordingObserver,
	errorKind string,
) {
	t.Helper()
	if len(observer.items) != 1 {
		t.Fatalf("observation count = %d, want 1: %+v", len(observer.items), observer.items)
	}
	if observer.items[0].Success || observer.items[0].ErrorKind != errorKind {
		t.Fatalf("failure observation = %+v, want error kind %q", observer.items[0], errorKind)
	}
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
	executor, err := runner.NewExecutor(registry.NewToolRegistry())
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
		snapshot:  toolLoopSnapshot("v1"),
		execution: newToolLoopTestExecution(t),
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
	prompt := model.requests[0].Messages[1].Content
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
		Template: "final {{.Query}} {{.Analysis}} {{.ToolHistory}}",
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
	execution := newToolLoopTestExecution(t)

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
		request.Messages[1].Content != "请根据工具结果回答" {
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
		execution:   newToolLoopTestExecution(t),
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
