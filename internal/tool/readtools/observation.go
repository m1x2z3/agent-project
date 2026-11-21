package readtools

import (
	"context"
	"errors"
	"time"

	"agent-project/internal/tool/tooling"
)

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
