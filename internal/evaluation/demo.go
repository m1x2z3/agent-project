package evaluation

import (
	"context"
	"time"

	"agent-project/internal/agent"
	"agent-project/internal/configregistry"
	"agent-project/internal/rollout"
)

// RunReleaseDemo 演示发布、灰度、影子运行、门禁、回滚和历史重放。
//
// 演示使用确定性 Fake Provider，不访问真实模型服务。
func RunReleaseDemo(ctx context.Context, testCase Case, candidateSummary Summary) (DemoResult, error) {
	registry := configregistry.NewRegistry()
	v1 := OfflineSnapshot("v1", "deepseek")
	v2 := OfflineSnapshot("v2", "kimi")
	if err := registry.Publish(v1); err != nil {
		return DemoResult{}, err
	}
	if err := registry.Publish(v2); err != nil {
		return DemoResult{}, err
	}
	if err := registry.Activate("v1"); err != nil {
		return DemoResult{}, err
	}
	selector, err := rollout.NewSelector(registry, rollout.RolloutPolicy{
		CandidateVersion: "v2",
		Percentage:       0,
		TenantAllowlist:  map[string]struct{}{"demo-tenant": {}},
	})
	if err != nil {
		return DemoResult{}, err
	}

	mainRuntime, err := newSharedRuntime(
		registry,
		selector,
		NewOfflineMultiProviderModel(testCase, map[string]bool{"kimi": true}),
	)
	if err != nil {
		return DemoResult{}, err
	}
	mainOutput, mainErr := mainRuntime.Run(ctx, agent.RunInput{
		RequestID: "demo-main",
		TenantID:  "normal-tenant",
		RunID:     "demo-main-run",
		Query:     testCase.Query,
	})

	_, shadowErr := mainRuntime.RunShadow(ctx, agent.RunInput{
		RequestID: "demo-shadow",
		TenantID:  "normal-tenant",
		RunID:     "demo-shadow-run",
		Query:     testCase.Query,
	})

	selection, err := selector.Select(rollout.SelectionInput{
		RequestID: "demo-allowlist",
		TenantID:  "demo-tenant",
	})
	if err != nil {
		return DemoResult{}, err
	}

	// 影子运行的候选失败会被转换为发布门禁的 block；
	// 回滚操作由演示流程显式调用，不隐藏在 EvaluateGate 内部。
	decision := rollout.EvaluateGate(candidateSummary.GateMetrics(), rollout.GatePolicy{
		MinSamples:               candidateSummary.GateMetrics().Samples,
		MinSuccessRate:           0.95,
		MinStructuredSuccessRate: 0.95,
		MaxP95Latency:            time.Second,
		MaxAverageCost:           0.01,
	})

	demo := DemoResult{
		MainReleaseVersion:   mainOutput.ReleaseVersion,
		MainSuccess:          mainErr == nil,
		ShadowReleaseVersion: "v2",
		ShadowSuccess:        shadowErr == nil,
		SelectionVersion:     selection.ReleaseVersion,
		SelectionCandidate:   selection.Candidate,
		SelectionReason:      selection.Reason,
		GateAction:           decision.Action,
		GateReason:           decision.Reason,
	}
	if shadowErr != nil {
		demo.ShadowErrorKind = ClassifyError(shadowErr)
	}

	if decision.Action == rollout.GateRollbackCandidate || decision.Action == rollout.GateBlock {
		// 先模拟候选版本被错误激活，再显式回滚到已验证的 v1，
		// 这样 PreviousVersion 和 RollbackFrom 才能完整记录发布关系。
		if err := registry.Activate("v2"); err != nil {
			return DemoResult{}, err
		}
		rollback, rollbackErr := registry.Rollback("v1", "v3", "offline-evaluation", time.Unix(2, 0).UTC())
		if rollbackErr != nil {
			return DemoResult{}, rollbackErr
		}
		demo.RollbackVersion = rollback.ReleaseVersion
		demo.PreviousVersion = rollback.Metadata.PreviousVersion
		demo.RollbackFrom = rollback.Metadata.RollbackFrom
	}
	for _, metadata := range registry.History() {
		demo.HistoryVersions = append(demo.HistoryVersions, metadata.Version)
	}

	replay, replayErr := replayHistoricalRun(ctx, registry, testCase)
	if replayErr != nil {
		return DemoResult{}, replayErr
	}
	demo.ReplayMatch = replay
	return demo, nil
}

func newSharedRuntime(
	registry *configregistry.Registry,
	selector *rollout.Selector,
	model agent.ModelCaller,
) (*agent.Runtime, error) {
	return agent.NewRuntime(agent.RuntimeConfig{
		Registry: registry,
		Selector: selector,
		Model:    model,
	})
}

func replayHistoricalRun(ctx context.Context, registry *configregistry.Registry, testCase Case) (bool, error) {
	snapshot, err := registry.Get("v1")
	if err != nil {
		return false, err
	}
	replayRegistry := configregistry.NewRegistry()
	if err := replayRegistry.Publish(snapshot); err != nil {
		return false, err
	}
	if err := replayRegistry.Activate("v1"); err != nil {
		return false, err
	}
	selector, err := rollout.NewSelector(replayRegistry, rollout.RolloutPolicy{
		CandidateVersion: "v1",
		Percentage:       100,
	})
	if err != nil {
		return false, err
	}
	runtime, err := newSharedRuntime(
		replayRegistry,
		selector,
		NewOfflineModel("deepseek", testCase, false),
	)
	if err != nil {
		return false, err
	}
	output, runErr := runtime.Run(ctx, agent.RunInput{
		RequestID: "demo-replay",
		TenantID:  "normal-tenant",
		RunID:     "demo-replay-run",
		Query:     testCase.Query,
	})
	if runErr != nil {
		return false, runErr
	}
	return output.ReleaseVersion == snapshot.ReleaseVersion &&
		output.AnalysisReport.Metadata.PromptVersion == snapshot.Prompts["task_analysis"].Version &&
		output.FinalAnswerReport.Metadata.SchemaVersion == snapshot.Schemas["final_answer"].Version, nil
}
