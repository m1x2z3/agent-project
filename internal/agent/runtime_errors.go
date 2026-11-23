package agent

import "errors"

var (
	ErrInvalidRunInput      = errors.New("agent run input is invalid")
	ErrRuntimeNotConfigured = errors.New("agent runtime is not configured")
	ErrPromptNotFound       = errors.New("agent runtime prompt not found")
	ErrSchemaNotFound       = errors.New("agent runtime schema not found")
	ErrRouteNotFound        = errors.New("agent runtime route not found")
	ErrPromptRenderFailed   = errors.New("agent runtime prompt render failed")
	ErrAnalysisDecodeFailed = errors.New("agent runtime analysis decode failed")
	ErrFinalDecodeFailed    = errors.New("agent runtime final answer decode failed")
	ErrAnalysisModelFailed  = errors.New("agent runtime analysis model failed")
	ErrFinalModelFailed     = errors.New("agent runtime final model failed")
)

type runtimeError struct {
	// runtimeError 记录失败发生在哪个阶段，同时保留原始错误，
	// 使调用方仍可通过 errors.Is/errors.As 判断取消、超时和具体错误类型。
	stage string
	cause error
}

func (e *runtimeError) Error() string {
	return "agent runtime failed at " + e.stage
}

func (e *runtimeError) Unwrap() error {
	return e.cause
}

func runtimeFailure(stage string, cause error) error {
	if cause == nil {
		return nil
	}
	return &runtimeError{stage: stage, cause: cause}
}
