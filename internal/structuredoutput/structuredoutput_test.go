package structuredoutput

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDecodeTaskAnalysis(t *testing.T) {
	options := testOptions(TaskAnalysisSchemaVersion)
	value, report, err := DecodeTaskAnalysis(context.Background(), []byte(`{"summary":"定位日志","confidence":0.9}`), options)
	if err != nil {
		t.Fatalf("DecodeTaskAnalysis() error = %v", err)
	}
	if value.Summary != "定位日志" || value.Confidence != 0.9 {
		t.Fatalf("unexpected value: %+v", value)
	}
	if report.Repaired || report.Attempts != 1 || !reflect.DeepEqual(report.Metadata, options.Metadata) {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestDecodeToolDecision(t *testing.T) {
	value, _, err := DecodeToolDecision(context.Background(), []byte(`{"should_call":true,"tool_name":"get_issue","arguments":{"id":"ISSUE-1"}}`), testOptions(ToolDecisionSchemaVersion))
	if err != nil {
		t.Fatalf("DecodeToolDecision() error = %v", err)
	}
	if !value.ShouldCall || value.ToolName != "get_issue" || value.Arguments["id"] != "ISSUE-1" {
		t.Fatalf("unexpected value: %+v", value)
	}
}

func TestDecodeFinalAnswerAndRefusal(t *testing.T) {
	t.Run("answered", func(t *testing.T) {
		value, _, err := DecodeFinalAnswer(context.Background(), []byte(`{"status":"answered","answer":"结论","citations":[{"source":"issue-1","location":"line 3"}]}`), testOptions(FinalAnswerSchemaVersion))
		if err != nil {
			t.Fatalf("DecodeFinalAnswer() error = %v", err)
		}
		if value.Status != FinalAnswerStatusAnswered || len(value.Citations) != 1 {
			t.Fatalf("unexpected value: %+v", value)
		}
	})
	t.Run("refused", func(t *testing.T) {
		value, _, err := DecodeFinalAnswer(context.Background(), []byte(`{"status":"refused","refusal_reason":"证据不足"}`), testOptions(FinalAnswerSchemaVersion))
		if err != nil {
			t.Fatalf("DecodeFinalAnswer() error = %v", err)
		}
		if value.Status != FinalAnswerStatusRefused || value.RefusalReason == "" {
			t.Fatalf("unexpected value: %+v", value)
		}
	})
}

func TestSchemaRejectsMissingWrongTypeAndExtraFields(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "missing required field", raw: `{"summary":"x"}`},
		{name: "wrong type", raw: `{"summary":"x","confidence":"high"}`},
		{name: "number out of range", raw: `{"summary":"x","confidence":1.1}`},
		{name: "extra field", raw: `{"summary":"x","confidence":0.5,"unexpected":true}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := DecodeTaskAnalysis(context.Background(), []byte(test.raw), testOptions(TaskAnalysisSchemaVersion))
			if !errors.Is(err, ErrRepairUnavailable) {
				t.Fatalf("error = %v, want repair unavailable after validation failure", err)
			}
		})
	}
}

func TestSchemaRejectsInvalidEnumAndNestedMissingFields(t *testing.T) {
	_, _, err := DecodeFinalAnswer(
		context.Background(),
		[]byte(`{"status":"unknown","answer":"x"}`),
		testOptions(FinalAnswerSchemaVersion),
	)
	if !errors.Is(err, ErrRepairUnavailable) {
		t.Fatalf("invalid enum error = %v", err)
	}

	_, _, err = DecodeFinalAnswer(
		context.Background(),
		[]byte(`{"status":"answered","answer":"x","citations":[{"source":"issue-1"}]}`),
		testOptions(FinalAnswerSchemaVersion),
	)
	if !errors.Is(err, ErrRepairUnavailable) {
		t.Fatalf("missing nested field error = %v", err)
	}
}

func TestGeneratedSchemaHasStableValidationRules(t *testing.T) {
	spec, err := schemaFor(OutputFinalAnswer)
	if err != nil {
		t.Fatalf("schemaFor() error = %v", err)
	}
	data, err := marshalSchema(spec)
	if err != nil {
		t.Fatalf("marshalSchema() error = %v", err)
	}

	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("generated schema is invalid JSON: %v", err)
	}
	if schema["$ref"] != "#/$defs/FinalAnswer" {
		t.Fatalf("root ref = %v, want FinalAnswer definition", schema["$ref"])
	}
	definitions, ok := schema["$defs"].(map[string]any)
	if !ok {
		t.Fatalf("generated schema has no $defs: %v", schema)
	}
	finalAnswer, ok := definitions["FinalAnswer"].(map[string]any)
	if !ok {
		t.Fatalf("FinalAnswer definition missing: %v", definitions)
	}
	if finalAnswer["additionalProperties"] != false {
		t.Fatalf("FinalAnswer additionalProperties = %v, want false", finalAnswer["additionalProperties"])
	}
	required, ok := finalAnswer["required"].([]any)
	if !ok || !containsString(required, "status") {
		t.Fatalf("required fields = %v, want status", finalAnswer["required"])
	}
	citation, ok := definitions["Citation"].(map[string]any)
	if !ok {
		t.Fatalf("Citation definition missing: %v", definitions)
	}
	if !reflect.DeepEqual(citation["required"], []any{"source", "location"}) {
		t.Fatalf("Citation required = %v", citation["required"])
	}
}

func containsString(values []any, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestStrictJSONRejectsInvalidAndTrailingJSON(t *testing.T) {
	_, _, err := DecodeTaskAnalysis(context.Background(), []byte(`{"summary":"x","confidence":0.5`), testOptions(TaskAnalysisSchemaVersion))
	if !errors.Is(err, ErrRepairUnavailable) {
		t.Fatalf("invalid JSON error = %v", err)
	}
	_, _, err = DecodeTaskAnalysis(context.Background(), []byte(`{"summary":"x","confidence":0.5}{"extra":true}`), testOptions(TaskAnalysisSchemaVersion))
	if !errors.Is(err, ErrRepairUnavailable) {
		t.Fatalf("trailing JSON error = %v", err)
	}
}

func TestBusinessSemanticValidation(t *testing.T) {
	tests := []struct {
		name string
		kind OutputKind
		raw  string
	}{
		{"summary is empty", OutputTaskAnalysis, `{"summary":" ","confidence":0.5}`},
		{"tool missing name", OutputToolDecision, `{"should_call":true,"arguments":{}}`},
		{"tool fields when disabled", OutputToolDecision, `{"should_call":false,"tool_name":"get_issue"}`},
		{"answered missing answer", OutputFinalAnswer, `{"status":"answered"}`},
		{"refused with answer", OutputFinalAnswer, `{"status":"refused","answer":"x","refusal_reason":"no"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var err error
			switch test.kind {
			case OutputTaskAnalysis:
				_, _, err = DecodeTaskAnalysis(context.Background(), []byte(test.raw), testOptions(TaskAnalysisSchemaVersion))
			case OutputToolDecision:
				_, _, err = DecodeToolDecision(context.Background(), []byte(test.raw), testOptions(ToolDecisionSchemaVersion))
			case OutputFinalAnswer:
				_, _, err = DecodeFinalAnswer(context.Background(), []byte(test.raw), testOptions(FinalAnswerSchemaVersion))
			}
			if !errors.Is(err, ErrRepairUnavailable) {
				t.Fatalf("error = %v, want semantic validation failure", err)
			}
		})
	}
}

func TestRepairRunsOnceAndMetadataIsPreserved(t *testing.T) {
	var calls atomic.Int32
	options := testOptions(FinalAnswerSchemaVersion)
	options.Repair = func(_ context.Context, request RepairRequest) ([]byte, error) {
		calls.Add(1)
		if request.Reason != "invalid_json" || request.Metadata.Model != "test-model" {
			t.Fatalf("unexpected repair request: %+v", request)
		}
		return []byte(`{"status":"answered","answer":"fixed"}`), nil
	}
	value, report, err := DecodeFinalAnswer(context.Background(), []byte(`not-json`), options)
	if err != nil {
		t.Fatalf("DecodeFinalAnswer() error = %v", err)
	}
	if value.Answer != "fixed" || !report.Repaired || report.Attempts != 2 || calls.Load() != 1 {
		t.Fatalf("unexpected value/report/calls: %+v %+v %d", value, report, calls.Load())
	}
}

func TestRepairFailureAndRejectedRepairDoNotRetry(t *testing.T) {
	var calls atomic.Int32
	options := testOptions(TaskAnalysisSchemaVersion)
	options.Repair = func(context.Context, RepairRequest) ([]byte, error) {
		calls.Add(1)
		return []byte(`{"summary":"" ,"confidence":0.5}`), nil
	}
	_, _, err := DecodeTaskAnalysis(context.Background(), []byte(`bad`), options)
	if !errors.Is(err, ErrRepairRejected) || calls.Load() != 1 {
		t.Fatalf("error = %v, calls = %d", err, calls.Load())
	}

	options.Repair = func(context.Context, RepairRequest) ([]byte, error) {
		calls.Add(1)
		return nil, errors.New("repair provider failed")
	}
	_, _, err = DecodeTaskAnalysis(context.Background(), []byte(`bad`), options)
	if !errors.Is(err, ErrRepairFailed) || calls.Load() != 2 {
		t.Fatalf("repair failure = %v, calls = %d", err, calls.Load())
	}
}

func TestDecodeContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := DecodeTaskAnalysis(ctx, []byte(`bad`), testOptions(TaskAnalysisSchemaVersion))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	options := testOptions(TaskAnalysisSchemaVersion)
	options.Repair = func(ctx context.Context, _ RepairRequest) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	_, _, err = DecodeTaskAnalysis(ctx, []byte(`bad`), options)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("repair cancellation error = %v", err)
	}
}

func TestErrorDoesNotExposeRawOutput(t *testing.T) {
	raw := `{"summary":"secret prompt and answer","confidence":"invalid"}`
	_, _, err := DecodeTaskAnalysis(context.Background(), []byte(raw), testOptions(TaskAnalysisSchemaVersion))
	if strings.Contains(err.Error(), "secret prompt and answer") || strings.Contains(err.Error(), "invalid") {
		t.Fatalf("error exposed raw output: %q", err)
	}
}

func testOptions(schemaVersion string) DecodeOptions {
	return DecodeOptions{
		Metadata: ExecutionMetadata{
			Provider:           "deepseek",
			Model:              "test-model",
			PromptVersion:      "prompt.v1",
			SchemaVersion:      schemaVersion,
			ModelConfigVersion: "model-config.v1",
		},
	}
}
