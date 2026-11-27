package readtools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"agent-project/internal/tool/tooling"
)

const (
	// SearchProjectDocsName 是项目文档搜索工具的稳定名称。
	SearchProjectDocsName = "search_project_docs"
)

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

// SearchProjectDocsParameterSchema 返回文档搜索工具的参数 Schema 副本。
func SearchProjectDocsParameterSchema() json.RawMessage {
	cloned := make(json.RawMessage, len(searchProjectDocsParameterSchema))
	copy(cloned, searchProjectDocsParameterSchema)
	return cloned
}

// SearchProjectDocsTool 在当前租户范围内搜索项目文档。
//
// 工具只依赖 ProjectDocStore，不直接依赖数据库、向量库或 HTTP 服务；
// 具体存储实现通过构造函数注入。
type SearchProjectDocsTool struct {
	store    ProjectDocStore
	observer Observer
}

// NewSearchProjectDocsTool 创建项目文档搜索工具。
func NewSearchProjectDocsTool(store ProjectDocStore, observer Observer) *SearchProjectDocsTool {
	if observer == nil {
		observer = NoopObserver{}
	}
	return &SearchProjectDocsTool{store: store, observer: observer}
}

// Definition 返回项目文档搜索工具的稳定契约。
func (t *SearchProjectDocsTool) Definition() tooling.ToolDefinition {
	return tooling.ToolDefinition{
		Name:              SearchProjectDocsName,
		Version:           ReadToolVersion,
		Description:       "在当前租户的指定项目中搜索研发文档",
		Permission:        tooling.Permission("project_docs:read"),
		RiskLevel:         tooling.RiskLow,
		EffectClass:       tooling.EffectRead,
		ResultContentKind: tooling.ResultContentDocument,
		Timeout:           2 * time.Second,
		ParameterSchema:   SearchProjectDocsParameterSchema(),
		TenantScope:       tooling.TenantScopeCurrentTenant,
		CapabilityTags:    []string{"read_only", "project_docs", "search"},
	}
}

// Execute 查询文档数据源并返回稳定结果。
//
// 参数 Schema、租户、Run、权限和能力令牌由 tooling.Invoke 负责；这里
// 只补充项目资源范围和搜索业务语义校验，再调用注入的数据源。
func (t *SearchProjectDocsTool) Execute(
	ctx context.Context,
	request tooling.ToolExecutionRequest,
) (result tooling.ToolResult, returnErr error) {
	start := time.Now()
	result = tooling.ToolResult{
		ToolName:    SearchProjectDocsName,
		ToolVersion: ReadToolVersion,
	}
	defer func() {
		observer := t.observer
		if isObservationSuppressed(ctx) {
			observer = NoopObserver{}
		}
		observeTool(observer, start, request, result, returnErr)
	}()

	if err := ctx.Err(); err != nil {
		return tooling.ToolResult{}, err
	}
	if t.store == nil {
		result.Status = tooling.ResultInternalError
		result.Error = &tooling.ToolError{
			Code:      "document_store_unavailable",
			Message:   "document store is unavailable",
			Retryable: true,
		}
		return result, nil
	}

	var arguments searchProjectDocsArguments
	if err := json.Unmarshal(request.Arguments, &arguments); err != nil {
		result.Status = tooling.ResultInvalidArgument
		result.Error = &tooling.ToolError{
			Code:      "invalid_argument",
			Message:   "search arguments are invalid",
			Retryable: false,
		}
		return result, nil
	}

	projectID := strings.TrimSpace(arguments.ProjectID)
	query := strings.TrimSpace(arguments.Query)
	if projectID == "" || query == "" || arguments.Limit < 1 || arguments.Limit > 20 {
		result.Status = tooling.ResultInvalidArgument
		result.Error = &tooling.ToolError{
			Code:      "invalid_argument",
			Message:   "project_id, query and limit are required",
			Retryable: false,
		}
		return result, nil
	}
	if !hasResource(request.ResourceScope, "project:"+projectID) {
		result.Status = tooling.ResultForbidden
		result.Error = &tooling.ToolError{
			Code:      "project_scope_denied",
			Message:   "project resource is not allowed",
			Retryable: false,
		}
		return result, nil
	}

	executionCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	hits, err := t.store.Search(executionCtx, request.TenantID, projectID, query, arguments.Limit)
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return tooling.ToolResult{}, err
		case errors.Is(err, context.DeadlineExceeded),
			errors.Is(err, ErrDocumentStoreTimeout):
			result.Status = tooling.ResultTimeout
			result.Error = &tooling.ToolError{
				Code:      "timeout",
				Message:   "document search timed out",
				Retryable: true,
			}
			return result, nil
		case errors.Is(err, ErrProjectNotFound):
			result.Status = tooling.ResultNotFound
			result.Error = &tooling.ToolError{
				Code:      "project_not_found",
				Message:   "project not found",
				Retryable: false,
			}
			return result, nil
		default:
			result.Status = tooling.ResultInternalError
			result.Error = &tooling.ToolError{
				Code:      "document_search_failed",
				Message:   "document search failed",
				Retryable: true,
			}
			return result, nil
		}
	}
	if err := executionCtx.Err(); err != nil {
		return tooling.ToolResult{}, err
	}

	data, err := json.Marshal(searchProjectDocsResult{
		ProjectID: projectID,
		Query:     query,
		Items:     hits,
	})
	if err != nil {
		result.Status = tooling.ResultInternalError
		result.Error = &tooling.ToolError{
			Code:      "result_encoding_failed",
			Message:   "failed to encode tool result",
			Retryable: false,
		}
		return result, nil
	}

	result.Status = tooling.ResultSuccess
	result.Data = data
	result.Source = make([]tooling.SourceRef, 0, len(hits))
	for _, hit := range hits {
		if strings.TrimSpace(hit.Source) == "" && strings.TrimSpace(hit.Location) == "" {
			continue
		}
		result.Source = append(result.Source, tooling.SourceRef{
			Source:   hit.Source,
			Location: hit.Location,
		})
	}
	return result, nil
}

type searchProjectDocsArguments struct {
	ProjectID string `json:"project_id"`
	Query     string `json:"query"`
	Limit     int    `json:"limit"`
}

type searchProjectDocsResult struct {
	ProjectID string        `json:"project_id"`
	Query     string        `json:"query"`
	Items     []DocumentHit `json:"items"`
}

func hasResource(resources []string, target string) bool {
	for _, resource := range resources {
		if strings.TrimSpace(resource) == target {
			return true
		}
	}
	return false
}

var _ tooling.Tool = (*SearchProjectDocsTool)(nil)
