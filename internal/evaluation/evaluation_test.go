package evaluation

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agent-project/internal/agent"
)

func TestLoadCasesRejectsMalformedAndDuplicateData(t *testing.T) {
	_, err := LoadCases(strings.NewReader(`{"id":"one","query":"q","expected_output_kind":"final_answer","expected_final_status":"answered","scenario":"normal"}
{"id":"one","query":"q","expected_output_kind":"final_answer","expected_final_status":"answered","scenario":"normal"}`))
	if err == nil {
		t.Fatal("LoadCases() error = nil, want duplicate id error")
	}

	_, err = LoadCases(strings.NewReader(`{"id":"one","query":"q","expected_output_kind":"final_answer"}`))
	if err == nil {
		t.Fatal("LoadCases() error = nil, want missing field error")
	}
}

func TestRunnerEvaluatesRepairAndFailures(t *testing.T) {
	cases := []Case{
		{
			ID: "normal", Query: "正常问题", ExpectedOutputKind: "final_answer",
			ExpectedFinalStatus: "answered", Scenario: ScenarioNormal, Quality: true,
		},
		{
			ID: "repair", Query: "修复问题", ExpectedOutputKind: "final_answer",
			ExpectedFinalStatus: "answered", AllowRepair: true, Scenario: ScenarioRepair, Quality: true,
		},
		{
			ID: "schema", Query: "结构问题", ExpectedOutputKind: "task_analysis",
			ExpectedFinalStatus: "answered", Scenario: ScenarioSchemaFailure,
		},
		{
			ID: "cancel", Query: "取消问题", ExpectedOutputKind: "final_answer",
			ExpectedFinalStatus: "answered", Scenario: ScenarioCanceled,
		},
	}
	runner := Runner{Repetitions: 2, Factory: NewOfflineRuntimeFactory()}
	summary, records, err := runner.Evaluate(context.Background(), "deepseek", "v1", cases)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if summary.Samples != 8 || summary.Successes != 4 || summary.StructuredSuccesses != 4 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.ParseFailures != 2 || summary.RepairCount != 2 {
		t.Fatalf("failure metrics = %+v", summary)
	}
	if metrics := summary.GateMetrics(); metrics.Samples != 4 || metrics.Successes != 4 ||
		metrics.StructuredSuccesses != 4 {
		t.Fatalf("gate metrics = %+v", metrics)
	}
	if len(records) != 8 || summary.StabilityRate != 1 {
		t.Fatalf("records/stability = %d/%v", len(records), summary.StabilityRate)
	}
}

func TestRunnerIsStableAndDoesNotExposeOutput(t *testing.T) {
	testCase := Case{
		ID: "stable", Query: "稳定问题", ExpectedOutputKind: "final_answer",
		ExpectedFinalStatus: "answered", Scenario: ScenarioNormal,
	}
	runner := Runner{Repetitions: 3, Factory: NewOfflineRuntimeFactory()}
	summary, records, err := runner.Evaluate(context.Background(), "kimi", "v1", []Case{testCase})
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if summary.StabilityRate != 1 || len(records[0].OutputDigest) != 64 {
		t.Fatalf("summary/record = %+v / %+v", summary, records[0])
	}
	for _, record := range records {
		if strings.Contains(record.OutputDigest, "稳定问题") || strings.Contains(record.OutputDigest, "离线回答") {
			t.Fatalf("record exposed raw output: %+v", record)
		}
	}
}

func TestOutputDigestChangesWhenStructuredResultChanges(t *testing.T) {
	first := agent.RunOutput{Provider: "deepseek", Model: "offline"}
	second := first
	second.FinalAnswer.Answer = "different"
	if reflect.DeepEqual(OutputDigest(first), OutputDigest(second)) {
		t.Fatal("OutputDigest() did not change with output")
	}
}

func TestCaseValidationRejectsInconsistentFlags(t *testing.T) {
	testCase := Case{
		ID: "invalid", Query: "q", ExpectedOutputKind: "final_answer",
		ExpectedFinalStatus: "answered", Scenario: ScenarioModelError,
	}
	if err := testCase.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want inconsistent model error flags")
	}
	if errors.Is(testCase.Validate(), context.Canceled) {
		t.Fatal("case validation unexpectedly returned context error")
	}
}
