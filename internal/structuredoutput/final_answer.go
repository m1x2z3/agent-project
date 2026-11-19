package structuredoutput

import (
	"errors"
	"strings"
)

// Citation 表示最终回答引用的证据来源和位置。
type Citation struct {
	Source   string `json:"source" jsonschema:"required"`
	Location string `json:"location" jsonschema:"required"`
}

// FinalAnswer 表示正常回答或明确拒答这两种最终结果。
type FinalAnswer struct {
	Status        string     `json:"status" jsonschema:"required,enum=answered,enum=refused"`
	Answer        string     `json:"answer,omitempty"`
	Citations     []Citation `json:"citations,omitempty"`
	RefusalReason string     `json:"refusal_reason,omitempty"`
}

// Validate 校验 answered 和 refused 两种最终状态的互斥规则。
func (v FinalAnswer) Validate() error {
	switch v.Status {
	case FinalAnswerStatusAnswered:
		if strings.TrimSpace(v.Answer) == "" {
			return errors.New("answer must not be empty when status is answered")
		}
		if strings.TrimSpace(v.RefusalReason) != "" {
			return errors.New("refusal_reason must be omitted when status is answered")
		}
	case FinalAnswerStatusRefused:
		if strings.TrimSpace(v.RefusalReason) == "" {
			return errors.New("refusal_reason must not be empty when status is refused")
		}
		if strings.TrimSpace(v.Answer) != "" {
			return errors.New("answer must be omitted when status is refused")
		}
	default:
		return errors.New("status must be answered or refused")
	}
	for _, citation := range v.Citations {
		if strings.TrimSpace(citation.Source) == "" || strings.TrimSpace(citation.Location) == "" {
			return errors.New("citation fields must not be empty")
		}
	}
	if v.Status == FinalAnswerStatusRefused && len(v.Citations) > 0 {
		return errors.New("citations must be omitted when status is refused")
	}
	return nil
}
