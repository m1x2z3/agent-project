package evaluation

import (
	"time"

	"agent-project/internal/agent"
	"agent-project/internal/rollout"
)

const (
	ScenarioNormal          = "normal"
	ScenarioRepair          = "repair"
	ScenarioSchemaFailure   = "schema_failure"
	ScenarioSemanticFailure = "semantic_failure"
	ScenarioModelError      = "model_error"
	ScenarioCanceled        = "canceled"
)

// Case 是一条脱敏、可版本化的离线评测样例。
//
// Scenario 只描述测试场景，不保存 Prompt、模型原文或真实用户数据。
type Case struct {
	ID                  string `json:"id"`
	Query               string `json:"query"`
	ExpectedOutputKind  string `json:"expected_output_kind"`
	ExpectedFinalStatus string `json:"expected_final_status"`
	AllowRepair         bool   `json:"allow_repair"`
	SimulateModelError  bool   `json:"simulate_model_error"`
	Scenario            string `json:"scenario"`
	CandidateFailure    bool   `json:"candidate_failure"`
	Quality             bool   `json:"quality"`
}

// RunRecord 是单次样例执行的脱敏结果。
//
// OutputDigest 用于判断重复执行结果是否稳定，不保存完整模型输出。
type RunRecord struct {
	CaseID              string
	Provider            string
	ReleaseVersion      string
	Repetition          int
	Success             bool
	StructuredValid     bool
	ExpectedStatusMatch bool
	Repaired            bool
	Duration            time.Duration
	PromptTokens        int
	CompletionTokens    int
	TotalTokens         int
	Cost                float64
	FinalStatus         string
	OutputDigest        string
	ErrorKind           string
}

// Summary 是一个供应商和配置版本在整组评测样例上的汇总。
type Summary struct {
	Provider              string
	ReleaseVersion        string
	Samples               int
	Successes             int
	StructuredSuccesses   int
	ParseFailures         int
	RepairCount           int
	ExpectedStatusMatches int
	StabilityRate         float64
	P50Latency            time.Duration
	P95Latency            time.Duration
	PromptTokens          int
	CompletionTokens      int
	TotalTokens           int
	TotalCost             float64
	ErrorTypes            map[string]int
	qualitySamples        int
	qualitySuccesses      int
	qualityStructured     int
	qualityLatencies      []time.Duration
	qualityTotalCost      float64
}

// RuntimeHandle 是评测器使用的一次 Runtime 及其修复调用计数。
type RuntimeHandle struct {
	Runtime        *agent.Runtime
	RepairAttempts func() int
}

// RuntimeFactory 为每次样例执行创建隔离的 Runtime。
//
// 每条样例使用独立 Runtime，避免 Fake Provider 的场景状态泄漏到其他样例。
type RuntimeFactory func(
	provider string,
	releaseVersion string,
	testCase Case,
	observer agent.Observer,
) (RuntimeHandle, error)

// DemoResult 保存发布、灰度、影子运行、回滚和历史配置重跑比较的脱敏结果。
type DemoResult struct {
	MainReleaseVersion   string
	MainSuccess          bool
	ShadowReleaseVersion string
	ShadowSuccess        bool
	ShadowErrorKind      string
	SelectionVersion     string
	SelectionCandidate   bool
	SelectionReason      string
	GateAction           string
	GateReason           string
	RollbackVersion      string
	PreviousVersion      string
	RollbackFrom         string
	HistoryVersions      []string
	RerunConfigMatch     bool // 表示使用原配置重跑后，输出引用的配置版本是否一致；不表示确定性依赖重放。
}

// Report 是评测命令最终渲染为 Markdown 的完整数据。
type Report struct {
	GeneratedAt time.Time
	Dataset     string
	Repetitions int
	Summaries   []Summary
	Demo        DemoResult
}

// GateMetrics 返回只由质量样例组成的发布门禁输入。
//
// 负向样例仍然会进入错误分类和报告，但不会因为“预期失败”降低候选版本质量分数。
func (s Summary) GateMetrics() rollout.GateMetrics {
	return rollout.GateMetrics{
		Samples:             s.qualitySamples,
		Successes:           s.qualitySuccesses,
		StructuredSuccesses: s.qualityStructured,
		Latencies:           append([]time.Duration(nil), s.qualityLatencies...),
		TotalCost:           s.qualityTotalCost,
	}
}
