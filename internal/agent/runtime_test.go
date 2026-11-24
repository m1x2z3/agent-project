package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-project/internal/configregistry"
	"agent-project/internal/modelgateway"
	"agent-project/internal/rollout"
	"agent-project/internal/structuredoutput"
)

func TestRuntimeRunsVersionedChainAndObservesMetadata(t *testing.T) {
	registry := configregistry.NewRegistry()
	publishRuntimeSnapshot(t, registry, "v1", "分析：{{.Query}}", "回答：{{.Query}} / {{.Analysis.Summary}}")
	publishRuntimeSnapshot(t, registry, "v2", "候选分析：{{.Query}}", "候选回答：{{.Query}} / {{.Analysis.Summary}}")
	if err := registry.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(registry, rollout.RolloutPolicy{CandidateVersion: "v2", Percentage: 0})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	model := &fakeRuntimeModel{}
	observer := &recordingObserver{}
	runtime, err := NewRuntime(RuntimeConfig{
		Registry: registry,
		Selector: selector,
		Model:    model,
		Observer: observer,
	})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}

	output, err := runtime.Run(context.Background(), RunInput{
		RequestID: "req-1", TenantID: "tenant-1", RunID: "run-1", Query: "订单服务错误",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if output.ReleaseVersion != "v1" || output.Provider != "fake" || output.Model != "fake-final_answer" ||
		output.Analysis.Summary != "定位订单服务错误" ||
		output.FinalAnswer.Status != structuredoutput.FinalAnswerStatusAnswered ||
		output.FinalAnswer.Answer != "已定位" {
		t.Fatalf("unexpected output: %+v", output)
	}
	if output.AnalysisReport.Metadata.PromptVersion != "prompt.analysis.v1" ||
		output.FinalAnswerReport.Metadata.SchemaVersion != "final_answer.v1" {
		t.Fatalf("missing output metadata: %+v", output)
	}
	if len(model.requests) != 2 ||
		!strings.Contains(model.requests[0].Messages[0].Content, "订单服务错误") ||
		!strings.Contains(model.requests[1].Messages[0].Content, "定位订单服务错误") {
		t.Fatalf("unexpected model requests: %+v", model.requests)
	}
	if model.requests[0].Model != "fake-task_analysis" ||
		model.requests[1].Model != "fake-final_answer" ||
		model.requests[0].Temperature == nil ||
		*model.requests[0].Temperature != 0 ||
		model.requests[0].MaxTokens != nil ||
		model.requests[0].Timeout != time.Second {
		t.Fatalf("route options were not propagated: %+v", model.requests)
	}
	if len(observer.items) != 1 || !observer.items[0].Success ||
		observer.items[0].TotalTokens != 6 ||
		observer.items[0].Cost <= 0 {
		t.Fatalf("unexpected observation: %+v", observer.items)
	}
}

func TestRuntimeUsesProviderAndModelForCostLookup(t *testing.T) {
	registry := configregistry.NewRegistry()
	snapshot := runtimeSnapshot("v1", "analysis {{.Query}}", "final")
	snapshot.Pricing = map[string]configregistry.PricingVersion{
		"same-model-deepseek": {
			Provider: "deepseek", Model: "same-model", Version: "price.v1",
			Currency: "USD", InputPer1K: 1, OutputPer1K: 2,
		},
		"same-model-kimi": {
			Provider: "kimi", Model: "same-model", Version: "price.v1",
			Currency: "USD", InputPer1K: 10, OutputPer1K: 20,
		},
	}
	snapshot.Routes["task_analysis"] = configregistry.ModelRouteVersion{
		Task: "task_analysis", Version: "route.analysis.v1",
		Provider: "deepseek", Model: "same-model", Timeout: time.Second,
	}
	snapshot.Routes["final_answer"] = configregistry.ModelRouteVersion{
		Task: "final_answer", Version: "route.final.v1",
		Provider: "kimi", Model: "same-model", Timeout: time.Second,
	}
	if err := registry.Publish(snapshot); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := registry.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(registry, rollout.RolloutPolicy{
		CandidateVersion: "v1", Percentage: 100,
	})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	observer := &recordingObserver{}
	runtime, err := NewRuntime(RuntimeConfig{
		Registry: registry, Selector: selector, Model: &fakeRuntimeModel{},
		Observer: observer,
	})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	if _, err := runtime.Run(context.Background(), RunInput{
		RequestID: "req-cost", RunID: "run-cost", Query: "问题",
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(observer.items) != 1 || observer.items[0].Cost <= 0.02 {
		t.Fatalf("unexpected cost observation: %+v", observer.items)
	}
}

func TestRuntimeUsesCandidateSnapshotAndShadowDoesNotConflictWithMainRun(t *testing.T) {
	registry := configregistry.NewRegistry()
	publishRuntimeSnapshot(t, registry, "v1", "old {{.Query}}", "old answer")
	publishRuntimeSnapshot(t, registry, "v2", "new {{.Query}}", "new answer")
	if err := registry.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(registry, rollout.RolloutPolicy{CandidateVersion: "v2", Percentage: 100})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	model := &fakeRuntimeModel{}
	observer := &recordingObserver{}
	runtime, err := NewRuntime(RuntimeConfig{
		Registry: registry,
		Selector: selector,
		Model:    model,
		Observer: observer,
	})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}

	shadow, err := runtime.RunShadow(context.Background(), RunInput{
		RequestID: "req-shadow", RunID: "run-shadow", Query: "问题",
	})
	if err != nil {
		t.Fatalf("RunShadow() error = %v", err)
	}
	if !shadow.Shadow || shadow.ReleaseVersion != "v2" {
		t.Fatalf("shadow output = %+v", shadow)
	}
	if len(observer.items) != 1 || !observer.items[0].Shadow || !observer.items[0].Success {
		t.Fatalf("shadow observation = %+v", observer.items)
	}
	if _, err := registry.GetRunSnapshot("run-shadow"); !errors.Is(err, configregistry.ErrRunNotFound) {
		t.Fatalf("shadow run unexpectedly occupied original run ID: %v", err)
	}
}

func TestRuntimeBindsOldRunToOldPromptAfterActivationChanges(t *testing.T) {
	registry := configregistry.NewRegistry()
	publishRuntimeSnapshot(t, registry, "v1", "old {{.Query}}", "final")
	publishRuntimeSnapshot(t, registry, "v2", "new {{.Query}}", "final")
	if err := registry.Activate("v1"); err != nil {
		t.Fatalf("Activate(v1) error = %v", err)
	}
	selector, err := rollout.NewSelector(registry, rollout.RolloutPolicy{CandidateVersion: "v2", Percentage: 0})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	model := &fakeRuntimeModel{}
	runtime, err := NewRuntime(RuntimeConfig{Registry: registry, Selector: selector, Model: model})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}

	if _, err := runtime.Run(context.Background(), RunInput{
		RequestID: "req-old", RunID: "run-old", Query: "问题",
	}); err != nil {
		t.Fatalf("Run(old) error = %v", err)
	}
	if err := registry.Activate("v2"); err != nil {
		t.Fatalf("Activate(v2) error = %v", err)
	}
	if _, err := registry.GetRunSnapshot("run-old"); err != nil {
		t.Fatalf("GetRunSnapshot() error = %v", err)
	}
	if len(model.requests) < 2 || !strings.HasPrefix(model.requests[0].Messages[0].Content, "old ") {
		t.Fatalf("first analysis prompt = %+v", model.requests)
	}
	if _, err := runtime.Run(context.Background(), RunInput{
		RequestID: "req-new", RunID: "run-new", Query: "问题",
	}); err != nil {
		t.Fatalf("Run(new) error = %v", err)
	}
	if len(model.requests) < 3 || !strings.HasPrefix(model.requests[2].Messages[0].Content, "new ") {
		t.Fatalf("new analysis prompt = %+v", model.requests)
	}
}

func TestRuntimeStopsOnContextCancellationAndLimitsRepairToOnce(t *testing.T) {
	registry := configregistry.NewRegistry()
	publishRuntimeSnapshot(t, registry, "v1", "analysis {{.Query}}", "final")
	if err := registry.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(registry, rollout.RolloutPolicy{CandidateVersion: "v1", Percentage: 100})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	model := &fakeRuntimeModel{analysisContent: `{"summary":"","confidence":0.5}`}
	repairCalls := 0
	runtime, err := NewRuntime(RuntimeConfig{
		Registry: selectorRegistry(registry),
		Selector: selector,
		Model:    model,
		AnalysisRepair: func(ctx context.Context, request structuredoutput.RepairRequest) ([]byte, error) {
			repairCalls++
			return []byte(`{"summary":"修复后","confidence":0.8}`), nil
		},
	})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	output, err := runtime.Run(context.Background(), RunInput{
		RequestID: "req-repair", RunID: "run-repair", Query: "问题",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if output.Analysis.Summary != "修复后" || repairCalls != 1 {
		t.Fatalf("output=%+v repairCalls=%d", output, repairCalls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = runtime.Run(ctx, RunInput{RequestID: "req-cancel", RunID: "run-cancel", Query: "问题"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
}

func TestRuntimePropagatesModelAndTemplateErrors(t *testing.T) {
	registry := configregistry.NewRegistry()
	publishRuntimeSnapshot(t, registry, "v1", "{{.Missing}}", "final")
	if err := registry.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(registry, rollout.RolloutPolicy{CandidateVersion: "v1", Percentage: 100})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	expected := errors.New("model unavailable")
	runtime, err := NewRuntime(RuntimeConfig{
		Registry: registry,
		Selector: selector,
		Model:    &fakeRuntimeModel{err: expected},
	})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	_, err = runtime.Run(context.Background(), RunInput{RequestID: "req", RunID: "run", Query: "问题"})
	if !errors.Is(err, ErrPromptRenderFailed) {
		t.Fatalf("template error = %v, want ErrPromptRenderFailed", err)
	}

	publishRuntimeSnapshot(t, registry, "v2", "valid {{.Query}}", "final")
	if err := registry.Activate("v2"); err != nil {
		t.Fatalf("Activate(v2) error = %v", err)
	}
	selector, err = rollout.NewSelector(registry, rollout.RolloutPolicy{CandidateVersion: "v2", Percentage: 100})
	if err != nil {
		t.Fatalf("NewSelector(v2) error = %v", err)
	}
	runtime, err = NewRuntime(RuntimeConfig{Registry: registry, Selector: selector, Model: &fakeRuntimeModel{err: expected}})
	if err != nil {
		t.Fatalf("NewRuntime(v2) error = %v", err)
	}
	_, err = runtime.Run(context.Background(), RunInput{RequestID: "req-2", RunID: "run-2", Query: "问题"})
	if !errors.Is(err, ErrAnalysisModelFailed) {
		t.Fatalf("model error = %v, want ErrAnalysisModelFailed", err)
	}
}

func TestRuntimeRejectsMissingSchemaAndRouteConfiguration(t *testing.T) {
	tests := []struct {
		name      string
		prepare   func(configregistry.ConfigSnapshot)
		wantError error
	}{
		{
			name: "missing final schema",
			prepare: func(snapshot configregistry.ConfigSnapshot) {
				delete(snapshot.Schemas, "final_answer")
			},
			wantError: ErrSchemaNotFound,
		},
		{
			name: "missing analysis route",
			prepare: func(snapshot configregistry.ConfigSnapshot) {
				delete(snapshot.Routes, "task_analysis")
			},
			wantError: ErrRouteNotFound,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := configregistry.NewRegistry()
			snapshot := runtimeSnapshot("v1", "analysis {{.Query}}", "final")
			test.prepare(snapshot)
			if err := registry.Publish(snapshot); err != nil {
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
			runtime, err := NewRuntime(RuntimeConfig{
				Registry: registry,
				Selector: selector,
				Model:    &fakeRuntimeModel{},
			})
			if err != nil {
				t.Fatalf("NewRuntime() error = %v", err)
			}
			_, err = runtime.Run(context.Background(), RunInput{
				RequestID: "req-" + test.name,
				RunID:     "run-" + test.name,
				Query:     "问题",
			})
			if !errors.Is(err, test.wantError) {
				t.Fatalf("Run() error = %v, want %v", err, test.wantError)
			}
		})
	}
}

type fakeRuntimeModel struct {
	mu              sync.Mutex
	requests        []modelgateway.ChatRequest
	err             error
	analysisContent string
}

func (m *fakeRuntimeModel) Chat(ctx context.Context, request modelgateway.ChatRequest) (modelgateway.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return modelgateway.ChatResponse{}, err
	}
	m.mu.Lock()
	m.requests = append(m.requests, request)
	m.mu.Unlock()
	if m.err != nil {
		return modelgateway.ChatResponse{}, m.err
	}
	content := `{"summary":"定位订单服务错误","confidence":0.9}`
	if request.Task == "final_answer" {
		content = `{"status":"answered","answer":"已定位"}`
	}
	if request.Task == "tool_decision" {
		content = `{"should_call":false}`
	}
	if request.Task == "task_analysis" && m.analysisContent != "" {
		content = m.analysisContent
	}
	provider := request.Provider
	if provider == "" {
		provider = "fake"
	}
	model := request.Model
	if model == "" {
		model = "fake-" + request.Task
	}
	return modelgateway.ChatResponse{
		Provider: provider,
		Model:    model,
		Message:  modelgateway.Message{Role: "assistant", Content: content},
		Usage:    &modelgateway.Usage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
	}, nil
}

type recordingObserver struct {
	mu    sync.Mutex
	items []RunObservation
}

func (o *recordingObserver) Observe(observation RunObservation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.items = append(o.items, observation)
}

func publishRuntimeSnapshot(t *testing.T, registry *configregistry.Registry, version, analysisTemplate, finalTemplate string) {
	t.Helper()
	snapshot := runtimeSnapshot(version, analysisTemplate, finalTemplate)
	if err := registry.Publish(snapshot); err != nil {
		t.Fatalf("Publish(%s) error = %v", version, err)
	}
}

func runtimeSnapshot(version, analysisTemplate, finalTemplate string) configregistry.ConfigSnapshot {
	at := time.Now()
	return configregistry.ConfigSnapshot{
		ReleaseVersion: version,
		Prompts: map[string]configregistry.PromptVersion{
			"task_analysis": {Name: "task_analysis", Version: "prompt.analysis.v1", Template: analysisTemplate},
			"final_answer":  {Name: "final_answer", Version: "prompt.final.v1", Template: finalTemplate},
		},
		Schemas: map[string]configregistry.SchemaVersion{
			"task_analysis": {Kind: "task_analysis", Version: "task_analysis.v1"},
			"final_answer":  {Kind: "final_answer", Version: "final_answer.v1"},
		},
		Routes: map[string]configregistry.ModelRouteVersion{
			"task_analysis": {Task: "task_analysis", Version: "route.analysis.v1", Provider: "fake", Model: "fake-task_analysis", Timeout: time.Second},
			"final_answer":  {Task: "final_answer", Version: "route.final.v1", Provider: "fake", Model: "fake-final_answer", Timeout: time.Second},
		},
		Pricing: map[string]configregistry.PricingVersion{
			"fake-task_analysis": {Provider: "fake", Model: "fake-task_analysis", Version: "price.v1", Currency: "USD", InputPer1K: 1, OutputPer1K: 2},
			"fake-final_answer":  {Provider: "fake", Model: "fake-final_answer", Version: "price.v1", Currency: "USD", InputPer1K: 1, OutputPer1K: 2},
		},
		Metadata: configregistry.ReleaseMetadata{
			Version: version, PublishedBy: "test", PublishedAt: at, EffectiveAt: at,
		},
	}
}

func selectorRegistry(registry *configregistry.Registry) *configregistry.Registry {
	return registry
}
