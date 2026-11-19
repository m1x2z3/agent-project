package modelgateway

import (
	"context"
	"errors"
	"fmt"
)

// ErrorKind 表示模型网关错误分类。
type ErrorKind string

const (
	ErrorInvalidRequest      ErrorKind = "invalid_request"
	ErrorProviderNotFound    ErrorKind = "provider_not_found"
	ErrorProviderUnavailable ErrorKind = "provider_unavailable"
	ErrorCanceled            ErrorKind = "canceled"
	ErrorTimeout             ErrorKind = "timeout"
	ErrorNetwork             ErrorKind = "network_error"
	ErrorUpstream            ErrorKind = "upstream_error"
	ErrorInternal            ErrorKind = "internal_error"
)

var (
	ErrInvalidRequest      = errors.New("model gateway invalid request")
	ErrProviderNotFound    = errors.New("model gateway provider not found")
	ErrProviderUnavailable = errors.New("model gateway provider unavailable")
	ErrCanceled            = errors.New("model gateway request canceled")
	ErrTimeout             = errors.New("model gateway request timed out")
	ErrNetwork             = errors.New("model gateway network error")
	ErrUpstream            = errors.New("model gateway upstream error")
)

// Error 是不暴露供应商原始错误文本的统一错误。
type Error struct {
	Kind       ErrorKind
	Provider   string
	StatusCode int
	RequestID  string
}

func (e *Error) Error() string {
	if e.Provider == "" {
		return fmt.Sprintf("model gateway error: kind=%s", e.Kind)
	}
	return fmt.Sprintf("model gateway error: kind=%s provider=%s", e.Kind, e.Provider)
}

// Is 让调用方可以按错误类别判断，同时不需要依赖供应商 SDK 或错误字符串。
func (e *Error) Is(target error) bool {
	switch {
	case target == ErrInvalidRequest:
		return e.Kind == ErrorInvalidRequest
	case target == ErrProviderNotFound:
		return e.Kind == ErrorProviderNotFound
	case target == ErrProviderUnavailable:
		return e.Kind == ErrorProviderUnavailable
	case target == ErrCanceled:
		return e.Kind == ErrorCanceled
	case target == ErrTimeout:
		return e.Kind == ErrorTimeout
	case target == ErrNetwork:
		return e.Kind == ErrorNetwork
	case target == ErrUpstream:
		return e.Kind == ErrorUpstream
	case target == context.Canceled:
		return e.Kind == ErrorCanceled
	case target == context.DeadlineExceeded:
		return e.Kind == ErrorTimeout
	default:
		return false
	}
}
