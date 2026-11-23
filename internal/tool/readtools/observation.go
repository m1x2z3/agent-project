package readtools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"agent-project/internal/tool/tooling"
)

// ToolObservation 是一次只读工具调用的脱敏观测记录。
//
// DurationMS 使用整数毫秒，避免不同日志处理器对 time.Duration 字符串的解析
// 产生差异。ArgumentsDigest 只保存参数摘要，不保存原始参数。
type ToolObservation struct {
	ToolName        string               `json:"tool_name"`
	ToolVersion     string               `json:"tool_version"`
	RequestID       string               `json:"request_id"`
	DurationMS      int64                `json:"duration_ms"`
	ResultStatus    tooling.ResultStatus `json:"result_status"`
	ErrorCode       string               `json:"error_code,omitempty"`
	ArgumentsDigest string               `json:"arguments_digest"`
}

// Observer 接收只读工具的脱敏调用观测。
type Observer interface {
	Observe(ToolObservation)
}

// NoopObserver 是默认空观测器，避免工具调用方必须额外配置日志或指标系统。
type NoopObserver struct{}

// Observe 实现 Observer，但不保存任何调用数据。
func (NoopObserver) Observe(ToolObservation) {}

// observationSuppressedKey 标记当前调用由 InvokeWithObservation 统一记录。
//
// 直接调用 tooling.Invoke 时，具体工具仍保留自身观测能力；通过推荐入口
// 调用时，工具跳过内部记录，避免和入口层产生重复观测。
type observationSuppressedKey struct{}

func withObservationSuppressed(ctx context.Context) context.Context {
	return context.WithValue(ctx, observationSuppressedKey{}, true)
}

func isObservationSuppressed(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	suppressed, _ := ctx.Value(observationSuppressedKey{}).(bool)
	return suppressed
}

// observeTool 将工具执行结果转换成统一的脱敏观测。
//
// 工具自身的业务错误通过 ToolResult 表达；Context 取消或底层调用错误
// 可能只有 error，因此这里也负责为这类情况补充稳定的观测状态。
func observeTool(
	observer Observer,
	start time.Time,
	request tooling.ToolExecutionRequest,
	result tooling.ToolResult,
	err error,
) {
	if observer == nil {
		return
	}

	status := result.Status
	errorCode := ""
	if result.Error != nil {
		errorCode = result.Error.Code
	}
	if status == "" {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			status = tooling.ResultTimeout
			errorCode = "timeout"
		case errors.Is(err, context.Canceled):
			status = tooling.ResultInternalError
			errorCode = "canceled"
		case errors.Is(err, tooling.ErrInvalidRequest),
			errors.Is(err, tooling.ErrArgumentsInvalid):
			status = tooling.ResultInvalidArgument
			errorCode = "invalid_argument"
		case errors.Is(err, tooling.ErrTenantMismatch),
			errors.Is(err, tooling.ErrRunMismatch),
			errors.Is(err, tooling.ErrToolMismatch),
			errors.Is(err, tooling.ErrToolVersionMismatch),
			errors.Is(err, tooling.ErrCapabilityTokenMissing),
			errors.Is(err, tooling.ErrCapabilityTokenInvalid),
			errors.Is(err, tooling.ErrCapabilityExpired),
			errors.Is(err, tooling.ErrResourceScopeDenied),
			errors.Is(err, tooling.ErrPermissionDenied),
			errors.Is(err, tooling.ErrApprovalRequired):
			status = tooling.ResultForbidden
			errorCode = "forbidden"
		default:
			status = tooling.ResultInternalError
			if err != nil {
				errorCode = "execution_failed"
			}
		}
	}
	observer.Observe(ToolObservation{
		ToolName:        request.ToolName,
		ToolVersion:     request.ToolVersion,
		RequestID:       request.RequestID,
		DurationMS:      ObservationDuration(start),
		ResultStatus:    status,
		ErrorCode:       errorCode,
		ArgumentsDigest: digestArguments(request.Arguments),
	})
}

// digestArguments 只记录参数摘要，避免观测数据保存完整参数。
func digestArguments(arguments []byte) string {
	sum := sha256.Sum256(arguments)
	return hex.EncodeToString(sum[:])
}

// ObservationDuration 将耗时转换成统一的毫秒表示。
func ObservationDuration(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}
