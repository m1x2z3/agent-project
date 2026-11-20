package evaluation

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// LoadCases 从 JSONL 中读取评测样例，并逐行执行严格校验。
func LoadCases(reader io.Reader) ([]Case, error) {
	if reader == nil {
		return nil, fmt.Errorf("evaluation dataset reader is nil")
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	cases := make([]Case, 0)
	lineNumber := 0
	seen := make(map[string]struct{})
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var testCase Case
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&testCase); err != nil {
			return nil, fmt.Errorf("dataset line %d: invalid JSON", lineNumber)
		}
		var extra any
		if err := decoder.Decode(&extra); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("dataset line %d: trailing invalid JSON", lineNumber)
		} else if err == nil {
			return nil, fmt.Errorf("dataset line %d: multiple JSON values", lineNumber)
		}
		if err := testCase.Validate(); err != nil {
			return nil, fmt.Errorf("dataset line %d: %w", lineNumber, err)
		}
		if _, exists := seen[testCase.ID]; exists {
			return nil, fmt.Errorf("dataset line %d: duplicate case id", lineNumber)
		}
		seen[testCase.ID] = struct{}{}
		cases = append(cases, testCase)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read evaluation dataset: %w", err)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("evaluation dataset is empty")
	}
	return cases, nil
}

// Validate 确保样例能被评测器映射到明确的 Runtime 行为。
func (c Case) Validate() error {
	if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Query) == "" {
		return fmt.Errorf("case id and query are required")
	}
	switch c.ExpectedOutputKind {
	case "task_analysis", "final_answer":
	default:
		return fmt.Errorf("case %q has invalid expected_output_kind", c.ID)
	}
	switch c.ExpectedFinalStatus {
	case "answered", "refused":
	default:
		return fmt.Errorf("case %q has invalid expected_final_status", c.ID)
	}
	switch c.Scenario {
	case ScenarioNormal, ScenarioRepair, ScenarioSchemaFailure,
		ScenarioSemanticFailure, ScenarioModelError, ScenarioCanceled:
	default:
		return fmt.Errorf("case %q has invalid scenario", c.ID)
	}
	if c.SimulateModelError && c.Scenario != ScenarioModelError {
		return fmt.Errorf("case %q model error flag does not match scenario", c.ID)
	}
	if c.Scenario == ScenarioRepair && !c.AllowRepair {
		return fmt.Errorf("case %q repair scenario must allow repair", c.ID)
	}
	if c.Scenario == ScenarioModelError && !c.SimulateModelError {
		return fmt.Errorf("case %q model error scenario must set simulate_model_error", c.ID)
	}
	return nil
}
