package structuredoutput

import (
	"errors"
	"strings"
)

// TaskAnalysis 表示任务分类和分析阶段的结构化结果。
type TaskAnalysis struct {
	Summary    string  `json:"summary" jsonschema:"required"`
	Confidence float64 `json:"confidence" jsonschema:"required,minimum=0,maximum=1"`
}

// Validate 校验 TaskAnalysis 中无法仅靠 JSON Schema 表达的业务语义规则。
func (v TaskAnalysis) Validate() error {
	if strings.TrimSpace(v.Summary) == "" {
		return errors.New("summary must not be empty")
	}
	return nil
}
