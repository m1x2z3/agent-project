package readtools

import (
	"context"
	"encoding/json"
	"time"

	"agent-project/internal/tool/tooling"
)

const (
	// GetCurrentTimeName 是当前时间工具的稳定名称。
	GetCurrentTimeName = "get_current_time"
	// SearchProjectDocsName 是项目文档搜索工具的稳定名称。
	SearchProjectDocsName = "search_project_docs"
	// ReadToolVersion 是本阶段两个只读工具共用的初始版本。
	ReadToolVersion = "v1"
)

// DocumentHit 是文档数据源返回的一条脱敏检索结果。
//
// 工具只返回定位信息和摘要，不把完整文档内容放入统一工具结果，
// 这样可以控制上下文大小，也避免观测层意外记录完整文档。
type DocumentHit struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	Source   string `json:"source"`
	Location string `json:"location"`
}

// ProjectDocStore 是项目文档的只读数据源抽象。
//
// 具体实现可以是内存数据源、PostgreSQL/pgvector 适配器或后续的搜索服务，
// 工具本身不关心底层存储，只通过 tenantID 保持租户查询边界。
type ProjectDocStore interface {
	Search(
		context.Context,
		string,
		string,
		string,
		int,
	) ([]DocumentHit, error)
}

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

// GetCurrentTimeParameterSchema 返回当前时间工具的参数 Schema 副本。
//
// 返回副本而不是共享可变切片，避免调用方修改 Schema 后影响后续工具定义。
func GetCurrentTimeParameterSchema() json.RawMessage {
	return cloneRawMessage(getCurrentTimeParameterSchema)
}

// SearchProjectDocsParameterSchema 返回文档搜索工具的参数 Schema 副本。
func SearchProjectDocsParameterSchema() json.RawMessage {
	return cloneRawMessage(searchProjectDocsParameterSchema)
}

var getCurrentTimeParameterSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "timezone": {
      "type": "string",
      "minLength": 1,
      "description": "IANA 时区，例如 Asia/Shanghai"
    }
  },
  "required": ["timezone"],
  "additionalProperties": false
}`)

var searchProjectDocsParameterSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "project_id": {
      "type": "string",
      "minLength": 1
    },
    "query": {
      "type": "string",
      "minLength": 1
    },
    "limit": {
      "type": "integer",
      "minimum": 1,
      "maximum": 20
    }
  },
  "required": ["project_id", "query", "limit"],
  "additionalProperties": false
}`)

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	cloned := make(json.RawMessage, len(raw))
	copy(cloned, raw)
	return cloned
}

// ToolRequestContext 是两个只读工具共享的执行上下文。
//
// 具体工具仍然通过 tooling.Tool 接口接收完整请求；该结构只用于集中表达
// 工具实现需要读取的请求标识和租户信息，避免把授权逻辑散落到业务代码中。
type ToolRequestContext struct {
	TenantID  string
	RunID     string
	RequestID string
}

// ObservationDuration 将耗时转换成统一的毫秒表示。
func ObservationDuration(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}
