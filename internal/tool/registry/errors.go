package registry

import "errors"

var (
	// ErrInvalidRegistryTool 表示待注册工具为空或工具定义不符合契约。
	ErrInvalidRegistryTool = errors.New("registry tool is invalid")
	// ErrDuplicateTool 表示相同名称和版本的工具已经注册。
	ErrDuplicateTool = errors.New("tool is already registered")
	// ErrToolNotFound 表示注册中心中不存在指定名称的工具。
	ErrToolNotFound = errors.New("tool is not found")
	// ErrToolVersionNotFound 表示指定工具存在，但不存在请求的版本。
	ErrToolVersionNotFound = errors.New("tool version is not found")
	// ErrToolDeprecated 表示请求的工具版本已经废弃，不能作为可用工具解析。
	ErrToolDeprecated = errors.New("tool version is deprecated")
)
