package evaluation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"agent-project/internal/agent"
)

// Runner 执行同一批样例的多轮、供应商级离线评测。
type Runner struct {
	Repetitions int
	Factory     RuntimeFactory
}

// Evaluate 执行评测并返回汇总指标和脱敏的逐次记录。
func (r Runner) Evaluate(
	ctx context.Context,
	provider string,
	releaseVersion string,
	cases []Case,
) (Summary, []RunRecord, error) {
	if r.Factory == nil || provider == "" || releaseVersion == "" {
		return Summary{}, nil, errors.New("evaluation runner is not configured")
	}
	repetitions := r.Repetitions
	if repetitions <= 0 {
		repetitions = 3
	}
	for _, testCase := range cases {
		if err := testCase.Validate(); err != nil {
			return Summary{}, nil, err
		}
	}

	summary := Summary{
		Provider:       provider,
		ReleaseVersion: releaseVersion,
		Samples:        len(cases) * repetitions,
		ErrorTypes:     make(map[string]int),
	}
	records := make([]RunRecord, 0, summary.Samples)
	latencies := make([]time.Duration, 0, summary.Samples)

	for repetition := 1; repetition <= repetitions; repetition++ {
		for _, testCase := range cases {
			observer := &recordingObserver{}
			handle, err := r.Factory(provider, releaseVersion, testCase, observer)
			if err != nil {
				return Summary{}, nil, err
			}
			if handle.Runtime == nil {
				return Summary{}, nil, errors.New("evaluation runtime is nil")
			}

			runCtx := ctx
			var cancel context.CancelFunc
			if testCase.Scenario == ScenarioCanceled {
				runCtx, cancel = context.WithCancel(ctx)
				cancel()
			}
			started := time.Now()
			output, runErr := handle.Runtime.Run(runCtx, agent.RunInput{
				RequestID: "eval-" + provider + "-" + testCase.ID,
				TenantID:  "offline-evaluation",
				RunID:     "eval-" + provider + "-" + testCase.ID + "-" + formatRepetition(repetition),
				Query:     testCase.Query,
			})
			if cancel != nil {
				cancel()
			}

			record := RunRecord{
				CaseID:         testCase.ID,
				Provider:       provider,
				ReleaseVersion: releaseVersion,
				Repetition:     repetition,
				Duration:       time.Since(started),
				FinalStatus:    output.FinalAnswer.Status,
			}
			if observation, ok := observer.Last(); ok {
				record.PromptTokens = observation.PromptTokens
				record.CompletionTokens = observation.CompletionTokens
				record.TotalTokens = observation.TotalTokens
				record.Cost = observation.Cost
			}
			if handle.RepairAttempts != nil {
				record.Repaired = handle.RepairAttempts() > 0
			}
			if runErr == nil {
				record.Success = true
				record.StructuredValid = true
				record.ExpectedStatusMatch = output.FinalAnswer.Status == testCase.ExpectedFinalStatus
				record.Repaired = record.Repaired ||
					output.AnalysisReport.Repaired ||
					output.FinalAnswerReport.Repaired
				record.OutputDigest = OutputDigest(output)
				summary.Successes++
				summary.StructuredSuccesses++
				if record.ExpectedStatusMatch {
					summary.ExpectedStatusMatches++
				}
			} else {
				record.ErrorKind = ClassifyError(runErr)
				summary.ErrorTypes[record.ErrorKind]++
				if errors.Is(runErr, agent.ErrAnalysisDecodeFailed) ||
					errors.Is(runErr, agent.ErrFinalDecodeFailed) {
					summary.ParseFailures++
				}
			}
			if testCase.Quality {
				summary.qualitySamples++
				summary.qualityLatencies = append(summary.qualityLatencies, record.Duration)
				summary.qualityTotalCost += record.Cost
				if record.Success {
					summary.qualitySuccesses++
				}
				if record.StructuredValid {
					summary.qualityStructured++
				}
			}
			if record.Repaired {
				summary.RepairCount++
			}
			summary.PromptTokens += record.PromptTokens
			summary.CompletionTokens += record.CompletionTokens
			summary.TotalTokens += record.TotalTokens
			summary.TotalCost += record.Cost
			latencies = append(latencies, record.Duration)
			records = append(records, record)
		}
	}

	summary.P50Latency = percentile(latencies, 50)
	summary.P95Latency = percentile(latencies, 95)
	summary.StabilityRate = stabilityRate(records, repetitions)
	return summary, records, nil
}

// OutputDigest 只对结构化结果和版本元数据摘要，不保存完整模型原文。
func OutputDigest(output agent.RunOutput) string {
	value := struct {
		ReleaseVersion string
		Provider       string
		Model          string
		Analysis       any
		FinalAnswer    any
		AnalysisMeta   any
		FinalMeta      any
	}{
		ReleaseVersion: output.ReleaseVersion,
		Provider:       output.Provider,
		Model:          output.Model,
		Analysis:       output.Analysis,
		FinalAnswer:    output.FinalAnswer,
		AnalysisMeta:   output.AnalysisReport.Metadata,
		FinalMeta:      output.FinalAnswerReport.Metadata,
	}
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

type recordingObserver struct {
	observations []agent.RunObservation
}

func (o *recordingObserver) Observe(observation agent.RunObservation) {
	o.observations = append(o.observations, observation)
}

func (o *recordingObserver) Last() (agent.RunObservation, bool) {
	if len(o.observations) == 0 {
		return agent.RunObservation{}, false
	}
	return o.observations[len(o.observations)-1], true
}

func stabilityRate(records []RunRecord, repetitions int) float64 {
	if repetitions <= 0 {
		return 0
	}
	grouped := make(map[string][]string)
	for _, record := range records {
		if record.Success {
			grouped[record.CaseID] = append(grouped[record.CaseID], record.OutputDigest)
		}
	}
	if len(grouped) == 0 {
		return 0
	}
	stable := 0
	for _, digests := range grouped {
		if len(digests) != repetitions {
			continue
		}
		if allEqual(digests) {
			stable++
		}
	}
	return float64(stable) / float64(len(grouped))
}

func allEqual(values []string) bool {
	for _, value := range values[1:] {
		if value != values[0] {
			return false
		}
	}
	return true
}

func percentile(values []time.Duration, percentage int) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := (len(sorted)*percentage + 99) / 100
	if index < 1 {
		index = 1
	}
	if index > len(sorted) {
		index = len(sorted)
	}
	return sorted[index-1]
}

func formatRepetition(repetition int) string {
	return fmt.Sprintf("%02d", repetition)
}

// ClassifyError 将 Runtime 错误转换为稳定的报告分类。
func ClassifyError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, agent.ErrAnalysisDecodeFailed):
		return "analysis_decode"
	case errors.Is(err, agent.ErrFinalDecodeFailed):
		return "final_decode"
	case errors.Is(err, agent.ErrAnalysisModelFailed):
		return "analysis_model"
	case errors.Is(err, agent.ErrFinalModelFailed):
		return "final_model"
	default:
		return "runtime_error"
	}
}
