package tooling

import "errors"

var (
	// ErrInvalidDefinition 表示工具元数据不完整或包含不支持的枚举值。
	ErrInvalidDefinition = errors.New("tool definition is invalid")
	// ErrInvalidRequest 表示工具调用请求缺少必要字段或参数不是合法 JSON。
	ErrInvalidRequest = errors.New("tool execution request is invalid")
	// ErrArgumentsInvalid 表示调用参数没有通过工具定义的 JSON Schema。
	ErrArgumentsInvalid = errors.New("tool arguments are invalid")
	// ErrCapabilityTokenMissing 表示请求没有携带能力令牌。
	ErrCapabilityTokenMissing = errors.New("capability token is missing")
	// ErrCapabilityTokenInvalid 表示能力令牌缺少必要的绑定信息。
	ErrCapabilityTokenInvalid = errors.New("capability token is invalid")
	// ErrTenantMismatch 表示请求、令牌和工具允许范围之间的租户不一致。
	ErrTenantMismatch = errors.New("tenant does not match")
	// ErrRunMismatch 表示请求和能力令牌不属于同一个 Run。
	ErrRunMismatch = errors.New("run does not match")
	// ErrToolMismatch 表示请求和能力令牌绑定了其他工具。
	ErrToolMismatch = errors.New("tool does not match")
	// ErrToolVersionMismatch 表示请求和能力令牌绑定了其他工具版本。
	ErrToolVersionMismatch = errors.New("tool version does not match")
	// ErrCapabilityExpired 表示能力令牌已经过期。
	ErrCapabilityExpired = errors.New("capability token is expired")
	// ErrResourceScopeDenied 表示令牌没有覆盖当前请求访问的资源范围。
	ErrResourceScopeDenied = errors.New("resource scope is not allowed")
	// ErrPermissionDenied 表示令牌没有工具定义要求的权限。
	ErrPermissionDenied = errors.New("tool permission is denied")
	// ErrApprovalRequired 表示高风险副作用尚未经过审批，当前不能执行。
	ErrApprovalRequired = errors.New("tool execution requires approval")
	// ErrResultInvalid 表示工具返回的结果 Envelope 不符合统一契约。
	ErrResultInvalid = errors.New("tool result is invalid")
)
