package structuredoutput

import (
	"errors"
	"fmt"
)

type validationStage string

const (
	stageJSON     validationStage = "json"
	stageSchema   validationStage = "schema"
	stageSemantic validationStage = "semantic"
	stageRepair   validationStage = "repair"
	stageMetadata validationStage = "metadata"
)

var (
	ErrInvalidJSON        = errors.New("structured output is not valid JSON")
	ErrTrailingJSON       = errors.New("structured output contains trailing JSON")
	ErrSchemaValidation   = errors.New("structured output does not match schema")
	ErrSemanticValidation = errors.New("structured output violates business rules")
	ErrRepairUnavailable  = errors.New("structured output repair is unavailable")
	ErrRepairFailed       = errors.New("structured output repair failed")
	ErrRepairRejected     = errors.New("repaired structured output was rejected")
	ErrInvalidMetadata    = errors.New("structured output metadata is incomplete or invalid")
	ErrUnsupportedOutput  = errors.New("structured output kind is unsupported")
)

// DecodeError 暴露稳定的失败阶段，但不暴露模型原始输出。
type DecodeError struct {
	Kind  OutputKind
	Stage validationStage
	Cause error
}

func (e *DecodeError) Error() string {
	return fmt.Sprintf("decode %s structured output failed at %s", e.Kind, e.Stage)
}

func (e *DecodeError) Unwrap() error {
	return e.Cause
}
