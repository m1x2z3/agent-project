package evaluation

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"agent-project/internal/agent"
	"agent-project/internal/configregistry"
	"agent-project/internal/modelgateway"
	"agent-project/internal/rollout"
	"agent-project/internal/structuredoutput"
)

// OfflineModel 是不访问网络的确定性模型替身。
//
// Provider 为空时使用请求中的 Provider，便于演示一个 Runtime 跨版本切换供应商。
type OfflineModel struct {
	Provider          string
	TestCase          Case
	DegradedProviders map[string]bool
}

// NewOfflineModel 创建固定供应商的离线模型替身。
func NewOfflineModel(provider string, testCase Case, degraded bool) *OfflineModel {
	degradedProviders := make(map[string]bool)
	if degraded {
		degradedProviders[provider] = true
	}
	return &OfflineModel{
		Provider:          provider,
		TestCase:          testCase,
		DegradedProviders: degradedProviders,
	}
}

// NewOfflineMultiProviderModel 创建可根据请求 Provider 模拟不同结果的模型替身。
func NewOfflineMultiProviderModel(testCase Case, degradedProviders map[string]bool) *OfflineModel {
	copyMap := make(map[string]bool, len(degradedProviders))
	for provider, degraded := range degradedProviders {
		copyMap[provider] = degraded
	}
	return &OfflineModel{TestCase: testCase, DegradedProviders: copyMap}
}

// Chat 根据样例场景返回固定输出或固定错误。
func (m *OfflineModel) Chat(ctx context.Context, request modelgateway.ChatRequest) (modelgateway.ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return modelgateway.ChatResponse{}, err
	}
	provider := m.Provider
	if provider == "" {
		provider = request.Provider
	}
	if m.DegradedProviders[provider] && m.TestCase.CandidateFailure {
		return modelgateway.ChatResponse{
			Provider: provider,
			Model:    provider + "-offline",
			Message:  modelgateway.Message{Role: "assistant", Content: `{"summary":"","confidence":0.5}`},
			Usage:    offlineUsage(),
		}, nil
	}
	if m.TestCase.SimulateModelError || m.TestCase.Scenario == ScenarioModelError {
		return modelgateway.ChatResponse{}, errors.New("offline model simulated failure")
	}

	scenario := m.TestCase.Scenario
	switch request.Task {
	case "task_analysis":
		switch scenario {
		case ScenarioRepair:
			return offlineResponse(provider, `not-json`), nil
		case ScenarioSchemaFailure:
			return offlineResponse(provider, `{"summary":"分析完成","confidence":"invalid"}`), nil
		case ScenarioSemanticFailure:
			return offlineResponse(provider, `{"summary":"","confidence":0.5}`), nil
		default:
			return offlineResponse(provider, `{"summary":"离线定位订单服务错误","confidence":0.9}`), nil
		}
	case "final_answer":
		if scenario == ScenarioRepair {
			return offlineResponse(provider, `not-json`), nil
		}
		if m.TestCase.ExpectedFinalStatus == "refused" {
			return offlineResponse(provider, `{"status":"refused","refusal_reason":"离线证据不足"}`), nil
		}
		return offlineResponse(provider, `{"status":"answered","answer":"离线评测完成"}`), nil
	default:
		return modelgateway.ChatResponse{}, errors.New("offline model task is unsupported")
	}
}

// NewOfflineRuntimeFactory 返回 Runner 使用的默认离线 Runtime 工厂。
func NewOfflineRuntimeFactory() RuntimeFactory {
	return func(provider, releaseVersion string, testCase Case, observer agent.Observer) (RuntimeHandle, error) {
		registry := configregistry.NewRegistry()
		snapshot := OfflineSnapshot(releaseVersion, provider)
		if err := registry.Publish(snapshot); err != nil {
			return RuntimeHandle{}, err
		}
		if err := registry.Activate(releaseVersion); err != nil {
			return RuntimeHandle{}, err
		}
		selector, err := rollout.NewSelector(registry, rollout.RolloutPolicy{
			CandidateVersion: releaseVersion,
			Percentage:       0,
		})
		if err != nil {
			return RuntimeHandle{}, err
		}
		var repairCalls atomic.Int32
		var repair structuredoutput.RepairFunc
		if testCase.AllowRepair {
			repair = func(_ context.Context, request structuredoutput.RepairRequest) ([]byte, error) {
				repairCalls.Add(1)
				switch request.Kind {
				case structuredoutput.OutputTaskAnalysis:
					return []byte(`{"summary":"修复后的离线分析","confidence":0.8}`), nil
				case structuredoutput.OutputFinalAnswer:
					return []byte(`{"status":"answered","answer":"修复后的离线回答"}`), nil
				default:
					return nil, errors.New("offline repair kind unsupported")
				}
			}
		}
		model := NewOfflineModel(provider, testCase, releaseVersion != "v1")
		runtime, err := agent.NewRuntime(agent.RuntimeConfig{
			Registry:          registry,
			Selector:          selector,
			Model:             model,
			Observer:          observer,
			AnalysisRepair:    repair,
			FinalAnswerRepair: repair,
		})
		if err != nil {
			return RuntimeHandle{}, err
		}
		return RuntimeHandle{
			Runtime: runtime,
			RepairAttempts: func() int {
				return int(repairCalls.Load())
			},
		}, nil
	}
}

// OfflineSnapshot 创建评测所需的最小配置快照，不包含 API Key。
func OfflineSnapshot(version, provider string) configregistry.ConfigSnapshot {
	now := time.Unix(0, 0).UTC()
	model := provider + "-offline"
	return configregistry.ConfigSnapshot{
		ReleaseVersion: version,
		Prompts: map[string]configregistry.PromptVersion{
			"task_analysis": {
				Name: "task_analysis", Version: "prompt.analysis.v1",
				Template: "分析问题：{{.Query}}",
			},
			"final_answer": {
				Name: "final_answer", Version: "prompt.final.v1",
				Template: "基于分析 {{.Analysis}} 回答：{{.Query}}",
			},
		},
		Schemas: map[string]configregistry.SchemaVersion{
			"task_analysis": {Kind: "task_analysis", Version: structuredoutput.TaskAnalysisSchemaVersion},
			"final_answer":  {Kind: "final_answer", Version: structuredoutput.FinalAnswerSchemaVersion},
		},
		Routes: map[string]configregistry.ModelRouteVersion{
			"task_analysis": {
				Task: "task_analysis", Version: "route.analysis.v1",
				Provider: provider, Model: model, Timeout: time.Second,
			},
			"final_answer": {
				Task: "final_answer", Version: "route.final.v1",
				Provider: provider, Model: model, Timeout: time.Second,
			},
		},
		Pricing: map[string]configregistry.PricingVersion{
			model: {
				Provider: provider, Model: model, Version: "price.v1",
				Currency: "USD", InputPer1K: 0.001, OutputPer1K: 0.002,
			},
		},
		Metadata: configregistry.ReleaseMetadata{
			Version: version, PublishedBy: "offline-evaluation",
			PublishedAt: now, EffectiveAt: now,
		},
	}
}

func offlineResponse(provider, content string) modelgateway.ChatResponse {
	return modelgateway.ChatResponse{
		Provider: provider,
		Model:    provider + "-offline",
		Message:  modelgateway.Message{Role: "assistant", Content: content},
		Usage:    offlineUsage(),
	}
}

func offlineUsage() *modelgateway.Usage {
	return &modelgateway.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
}
