package runner

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agent-project/internal/structuredoutput"
	"agent-project/internal/tool/readtools"
	"agent-project/internal/tool/registry"
	"agent-project/internal/tool/tooling"
)

func TestToolCallRequestStoresDecisionAndExecutionContext(t *testing.T) {
	request := ToolCallRequest{
		Decision: structuredoutput.ToolDecision{
			ShouldCall: true,
			ToolName:   "get_current_time",
			Arguments:  map[string]any{"timezone": "Asia/Shanghai"},
		},
		ToolVersion:   "v1",
		TenantID:      "tenant-a",
		RunID:         "run-1",
		RequestID:     "request-1",
		ResourceScope: []string{"project:project-a"},
		CapabilityToken: tooling.CapabilityToken{
			TokenID:     "cap-1",
			TenantID:    "tenant-a",
			RunID:       "run-1",
			ToolName:    "get_current_time",
			ToolVersion: "v1",
			Nonce:       "nonce-1",
		},
		IdempotencyKey: "idempotency-1",
	}

	if !request.Decision.ShouldCall ||
		request.Decision.ToolName != "get_current_time" ||
		request.ToolVersion != "v1" ||
		request.TenantID != "tenant-a" ||
		request.RunID != "run-1" ||
		request.RequestID != "request-1" ||
		request.IdempotencyKey != "idempotency-1" {
		t.Fatalf("ToolCallRequest = %+v", request)
	}
	if request.CapabilityToken.ToolName != request.Decision.ToolName ||
		request.CapabilityToken.ToolVersion != request.ToolVersion {
		t.Fatalf("capability token binding = %+v", request.CapabilityToken)
	}
}

func TestExecutorStoresRegistryDependency(t *testing.T) {
	toolRegistry := registry.NewToolRegistry()
	executor := Executor{registry: toolRegistry}

	if executor.registry != toolRegistry {
		t.Fatal("Executor did not retain the registry dependency")
	}
}

func TestNewExecutorRequiresRegistry(t *testing.T) {
	if _, err := NewExecutor(nil); err != ErrExecutorNotConfigured {
		t.Fatalf("NewExecutor(nil) error = %v, want ErrExecutorNotConfigured", err)
	}
}

func TestNewExecutorCreatesConfiguredExecutor(t *testing.T) {
	toolRegistry := registry.NewToolRegistry()
	executor, err := NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	if executor == nil || executor.registry != toolRegistry {
		t.Fatal("NewExecutor() did not retain the registry dependency")
	}
}

func TestToolCallRequestValidate(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	request := validToolCallRequest(now.Add(time.Hour))

	if err := request.Validate(now); err != nil {
		t.Fatalf("valid ToolCallRequest rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*ToolCallRequest)
		want error
	}{
		{
			name: "decision must request a tool",
			edit: func(r *ToolCallRequest) {
				r.Decision = structuredoutput.ToolDecision{}
			},
			want: ErrInvalidToolDecision,
		},
		{
			name: "request id is required",
			edit: func(r *ToolCallRequest) {
				r.RequestID = ""
			},
			want: ErrInvalidToolCallRequest,
		},
		{
			name: "tenant id is required",
			edit: func(r *ToolCallRequest) {
				r.TenantID = ""
			},
			want: ErrInvalidToolCallRequest,
		},
		{
			name: "run id is required",
			edit: func(r *ToolCallRequest) {
				r.RunID = ""
			},
			want: ErrInvalidToolCallRequest,
		},
		{
			name: "tool version is required",
			edit: func(r *ToolCallRequest) {
				r.ToolVersion = ""
			},
			want: ErrToolVersionRequired,
		},
		{
			name: "capability token is required",
			edit: func(r *ToolCallRequest) {
				r.CapabilityToken = tooling.CapabilityToken{}
			},
			want: tooling.ErrCapabilityTokenMissing,
		},
		{
			name: "tenant must match capability token",
			edit: func(r *ToolCallRequest) {
				r.TenantID = "tenant-b"
			},
			want: tooling.ErrTenantMismatch,
		},
		{
			name: "run must match capability token",
			edit: func(r *ToolCallRequest) {
				r.RunID = "run-b"
			},
			want: tooling.ErrRunMismatch,
		},
		{
			name: "tool must match capability token",
			edit: func(r *ToolCallRequest) {
				r.Decision.ToolName = "other-tool"
			},
			want: tooling.ErrToolMismatch,
		},
		{
			name: "version must match capability token",
			edit: func(r *ToolCallRequest) {
				r.ToolVersion = "v2"
			},
			want: tooling.ErrToolVersionMismatch,
		},
		{
			name: "expired capability token",
			edit: func(r *ToolCallRequest) {
				r.CapabilityToken.ExpiresAt = now
			},
			want: tooling.ErrCapabilityExpired,
		},
		{
			name: "resource must be covered by capability token",
			edit: func(r *ToolCallRequest) {
				r.ResourceScope = []string{"project-b"}
			},
			want: tooling.ErrResourceScopeDenied,
		},
		{
			name: "resource cannot be empty",
			edit: func(r *ToolCallRequest) {
				r.ResourceScope = []string{""}
			},
			want: tooling.ErrResourceScopeDenied,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := request
			test.edit(&got)
			if !errors.Is(got.Validate(now), test.want) {
				t.Fatalf("Validate() = %v, want %v", got.Validate(now), test.want)
			}
		})
	}
}

func validToolCallRequest(expiresAt time.Time) ToolCallRequest {
	return ToolCallRequest{
		Decision: structuredoutput.ToolDecision{
			ShouldCall: true,
			ToolName:   "search_project_docs",
			Arguments:  map[string]any{"query": "error"},
		},
		ToolVersion:   "v1",
		TenantID:      "tenant-a",
		RunID:         "run-1",
		RequestID:     "request-1",
		ResourceScope: []string{"project-a"},
		CapabilityToken: tooling.CapabilityToken{
			TokenID:       "cap-1",
			TenantID:      "tenant-a",
			RunID:         "run-1",
			ToolName:      "search_project_docs",
			ToolVersion:   "v1",
			ResourceScope: []string{"project-a"},
			ExpiresAt:     expiresAt,
			Nonce:         "nonce-1",
		},
	}
}

func TestExecutorResolvesToolFromRequest(t *testing.T) {
	toolRegistry := registry.NewToolRegistry()
	tool := &runnerTestTool{definition: runnerTestDefinition()}
	if err := toolRegistry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	executor, err := NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}

	request := validToolCallRequest(time.Now().Add(time.Hour))
	registered, err := executor.resolveTool(context.Background(), request)
	if err != nil {
		t.Fatalf("resolveTool() error = %v", err)
	}
	if registered.Tool != tool {
		t.Fatal("resolveTool() returned a different tool instance")
	}
	if registered.Definition.Name != request.Decision.ToolName ||
		registered.Definition.Version != request.ToolVersion {
		t.Fatalf("resolved definition = %+v", registered.Definition)
	}
}

func TestExecutorResolveToolRejectsUnknownToolAndVersion(t *testing.T) {
	toolRegistry := registry.NewToolRegistry()
	if err := toolRegistry.Register(&runnerTestTool{definition: runnerTestDefinition()}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	executor, err := NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}

	unknownTool := validToolCallRequest(time.Now().Add(time.Hour))
	unknownTool.Decision.ToolName = "unknown_tool"
	unknownTool.CapabilityToken.ToolName = "unknown_tool"
	if _, err := executor.resolveTool(context.Background(), unknownTool); !errors.Is(err, registry.ErrToolNotFound) {
		t.Fatalf("unknown tool error = %v, want ErrToolNotFound", err)
	}

	unknownVersion := validToolCallRequest(time.Now().Add(time.Hour))
	unknownVersion.ToolVersion = "v2"
	unknownVersion.CapabilityToken.ToolVersion = "v2"
	if _, err := executor.resolveTool(context.Background(), unknownVersion); !errors.Is(err, registry.ErrToolVersionNotFound) {
		t.Fatalf("unknown version error = %v, want ErrToolVersionNotFound", err)
	}
}

func TestExecutorResolveToolDoesNotExecuteTool(t *testing.T) {
	tool := &runnerTestTool{definition: runnerTestDefinition()}
	toolRegistry := registry.NewToolRegistry()
	if err := toolRegistry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	executor, err := NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}

	if _, err := executor.resolveTool(context.Background(), validToolCallRequest(time.Now().Add(time.Hour))); err != nil {
		t.Fatalf("resolveTool() error = %v", err)
	}
	if tool.executeCalls != 0 {
		t.Fatalf("resolveTool() executed the tool: calls=%d", tool.executeCalls)
	}
}

func TestBuildExecutionRequest(t *testing.T) {
	request := validToolCallRequest(time.Now().Add(time.Hour))
	executionRequest, err := buildExecutionRequest(request)
	if err != nil {
		t.Fatalf("buildExecutionRequest() error = %v", err)
	}

	if executionRequest.ToolName != request.Decision.ToolName ||
		executionRequest.ToolVersion != request.ToolVersion ||
		executionRequest.TenantID != request.TenantID ||
		executionRequest.RunID != request.RunID ||
		executionRequest.RequestID != request.RequestID ||
		executionRequest.IdempotencyKey != request.IdempotencyKey {
		t.Fatalf("execution request context = %+v", executionRequest)
	}
	if string(executionRequest.Arguments) != `{"query":"error"}` {
		t.Fatalf("execution request arguments = %s", executionRequest.Arguments)
	}
	if len(executionRequest.ResourceScope) != 1 ||
		executionRequest.ResourceScope[0] != "project-a" {
		t.Fatalf("execution request resource scope = %v", executionRequest.ResourceScope)
	}
	if executionRequest.CapabilityToken.ToolVersion != request.ToolVersion {
		t.Fatalf("execution request capability token = %+v", executionRequest.CapabilityToken)
	}
}

func TestBuildExecutionRequestRejectsUnencodableArguments(t *testing.T) {
	request := validToolCallRequest(time.Now().Add(time.Hour))
	request.Decision.Arguments = map[string]any{
		"unsupported": func() {},
	}

	if _, err := buildExecutionRequest(request); !errors.Is(err, ErrArgumentsEncodingFailed) {
		t.Fatalf("buildExecutionRequest() error = %v, want ErrArgumentsEncodingFailed", err)
	}
}

func TestExecutorExecuteInvokesResolvedTool(t *testing.T) {
	tool := &runnerTestTool{definition: runnerTestDefinition()}
	toolRegistry := registry.NewToolRegistry()
	if err := toolRegistry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	executor, err := NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}

	request := validToolCallRequest(time.Now().Add(time.Hour))
	request.CapabilityToken.Permissions = []tooling.Permission{tooling.Permission("docs:read")}

	result, err := executor.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != tooling.ResultSuccess {
		t.Fatalf("Execute() status = %s, want %s", result.Status, tooling.ResultSuccess)
	}
	if result.ToolName != request.Decision.ToolName ||
		result.ToolVersion != request.ToolVersion {
		t.Fatalf("Execute() result = %+v", result)
	}
	if tool.executeCalls != 1 {
		t.Fatalf("Execute() calls = %d, want 1", tool.executeCalls)
	}
}

func TestExecutorExecuteRejectsRequestBeforeToolExecution(t *testing.T) {
	tool := &runnerTestTool{definition: runnerTestDefinition()}
	toolRegistry := registry.NewToolRegistry()
	if err := toolRegistry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	executor, err := NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}

	request := validToolCallRequest(time.Now().Add(time.Hour))
	request.CapabilityToken.Permissions = []tooling.Permission{tooling.Permission("docs:read")}
	request.TenantID = "tenant-b"

	if _, err := executor.Execute(context.Background(), request); !errors.Is(err, tooling.ErrTenantMismatch) {
		t.Fatalf("Execute() error = %v, want ErrTenantMismatch", err)
	}
	if tool.executeCalls != 0 {
		t.Fatalf("Execute() calls = %d, want 0", tool.executeCalls)
	}
}

func TestExecutorExecuteStopsWhenContextIsCanceled(t *testing.T) {
	tool := &runnerTestTool{definition: runnerTestDefinition()}
	toolRegistry := registry.NewToolRegistry()
	if err := toolRegistry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	executor, err := NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}

	request := validToolCallRequest(time.Now().Add(time.Hour))
	request.CapabilityToken.Permissions = []tooling.Permission{tooling.Permission("docs:read")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := executor.Execute(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want context.Canceled", err)
	}
	if tool.executeCalls != 0 {
		t.Fatalf("Execute() calls = %d, want 0", tool.executeCalls)
	}
}

func TestExecutorExecuteChecksContextBeforeResolvingTool(t *testing.T) {
	toolRegistry := registry.NewToolRegistry()
	if err := toolRegistry.Register(&runnerTestTool{definition: runnerTestDefinition()}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	executor, err := NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	request := validToolCallRequest(time.Now().Add(time.Hour))
	request.CapabilityToken.Permissions = []tooling.Permission{tooling.Permission("docs:read")}
	request.Decision.ToolName = "unknown_tool"
	request.CapabilityToken.ToolName = "unknown_tool"

	if _, err := executor.Execute(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("Execute() error = %v, want context.Canceled", err)
	}
}

func TestExecutorExecutePropagatesToolError(t *testing.T) {
	toolError := errors.New("tool backend unavailable")
	tool := &runnerTestTool{
		definition: runnerTestDefinition(),
		executeErr: toolError,
	}
	toolRegistry := registry.NewToolRegistry()
	if err := toolRegistry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	executor, err := NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}

	request := validToolCallRequest(time.Now().Add(time.Hour))
	request.CapabilityToken.Permissions = []tooling.Permission{tooling.Permission("docs:read")}

	result, err := executor.Execute(context.Background(), request)
	if !errors.Is(err, toolError) {
		t.Fatalf("Execute() error = %v, want %v", err, toolError)
	}
	if result.Status != "" {
		t.Fatalf("Execute() result = %+v, want zero result", result)
	}
	if tool.executeCalls != 1 {
		t.Fatalf("Execute() calls = %d, want 1", tool.executeCalls)
	}
}

func TestExecutorExecuteUsesToolingSchemaBoundary(t *testing.T) {
	tool := &runnerTestTool{definition: runnerTestDefinition()}
	toolRegistry := registry.NewToolRegistry()
	if err := toolRegistry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	executor, err := NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}

	request := validToolCallRequest(time.Now().Add(time.Hour))
	request.Decision.Arguments = map[string]any{"unexpected": true}
	request.CapabilityToken.Permissions = []tooling.Permission{tooling.Permission("docs:read")}

	if _, err := executor.Execute(context.Background(), request); !errors.Is(err, tooling.ErrArgumentsInvalid) {
		t.Fatalf("Execute() error = %v, want ErrArgumentsInvalid", err)
	}
	if tool.executeCalls != 0 {
		t.Fatalf("Execute() calls = %d, want 0", tool.executeCalls)
	}
}

func TestExecutorExecuteUsesEffectBoundary(t *testing.T) {
	definition := runnerTestDefinition()
	definition.Name = "deploy_service"
	definition.Description = "部署服务"
	definition.Permission = "deploy:write"
	definition.RiskLevel = tooling.RiskHigh
	definition.EffectClass = tooling.EffectNonRetryableWrite
	tool := &runnerTestTool{definition: definition}
	toolRegistry := registry.NewToolRegistry()
	if err := toolRegistry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	executor, err := NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}

	request := validToolCallRequest(time.Now().Add(time.Hour))
	request.Decision.ToolName = definition.Name
	request.CapabilityToken.ToolName = definition.Name
	request.CapabilityToken.Permissions = []tooling.Permission{definition.Permission}

	if _, err := executor.Execute(context.Background(), request); !errors.Is(err, tooling.ErrApprovalRequired) {
		t.Fatalf("Execute() error = %v, want ErrApprovalRequired", err)
	}
	if tool.executeCalls != 0 {
		t.Fatalf("Execute() calls = %d, want 0", tool.executeCalls)
	}
}

func TestExecutorExecuteRunsRegisteredReadTool(t *testing.T) {
	// 使用相对当前时间的 Token，避免测试在日期变化后误判为过期。
	now := time.Now().UTC()
	tool := readtools.NewCurrentTimeTool(func() time.Time { return now }, nil)
	toolRegistry := registry.NewToolRegistry()
	if err := toolRegistry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	executor, err := NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}

	request := ToolCallRequest{
		Decision: structuredoutput.ToolDecision{
			ShouldCall: true,
			ToolName:   readtools.GetCurrentTimeName,
			Arguments:  map[string]any{"timezone": "Asia/Shanghai"},
		},
		ToolVersion: readtools.ReadToolVersion,
		TenantID:    "tenant-a",
		RunID:       "run-1",
		RequestID:   "request-1",
		CapabilityToken: tooling.CapabilityToken{
			TokenID:     "cap-1",
			TenantID:    "tenant-a",
			RunID:       "run-1",
			ToolName:    readtools.GetCurrentTimeName,
			ToolVersion: readtools.ReadToolVersion,
			Permissions: []tooling.Permission{"time:read"},
			ExpiresAt:   now.Add(time.Hour),
			Nonce:       "nonce-1",
		},
	}

	result, err := executor.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != tooling.ResultSuccess {
		t.Fatalf("Execute() status = %s, want %s", result.Status, tooling.ResultSuccess)
	}

	var payload struct {
		Timezone string `json:"timezone"`
	}
	if err := json.Unmarshal(result.Data, &payload); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	if payload.Timezone != "Asia/Shanghai" {
		t.Fatalf("tool result timezone = %q", payload.Timezone)
	}
}

func TestExecutorExecuteRunsProjectDocsToolWithTenantScope(t *testing.T) {
	store := readtools.NewMemoryProjectDocStore([]readtools.ProjectDocument{
		{
			TenantID:  "tenant-a",
			ProjectID: "project-a",
			Hit:       readtools.DocumentHit{ID: "doc-a", Summary: "订单服务错误"},
		},
		{
			TenantID:  "tenant-b",
			ProjectID: "project-a",
			Hit:       readtools.DocumentHit{ID: "doc-b", Summary: "订单服务错误"},
		},
	})
	tool := readtools.NewSearchProjectDocsTool(store, nil)
	toolRegistry := registry.NewToolRegistry()
	if err := toolRegistry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	executor, err := NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}

	request := ToolCallRequest{
		Decision: structuredoutput.ToolDecision{
			ShouldCall: true,
			ToolName:   readtools.SearchProjectDocsName,
			Arguments: map[string]any{
				"project_id": "project-a",
				"query":      "订单服务错误",
				"limit":      10,
			},
		},
		ToolVersion:   readtools.ReadToolVersion,
		TenantID:      "tenant-a",
		RunID:         "run-1",
		RequestID:     "request-docs-1",
		ResourceScope: []string{"project:project-a"},
		CapabilityToken: tooling.CapabilityToken{
			TokenID:       "cap-docs-1",
			TenantID:      "tenant-a",
			RunID:         "run-1",
			ToolName:      readtools.SearchProjectDocsName,
			ToolVersion:   readtools.ReadToolVersion,
			ResourceScope: []string{"project:project-a"},
			Permissions:   []tooling.Permission{"project_docs:read"},
			ExpiresAt:     time.Now().Add(time.Hour),
			Nonce:         "nonce-docs-1",
		},
	}

	result, err := executor.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.Status != tooling.ResultSuccess {
		t.Fatalf("Execute() status = %s, want %s", result.Status, tooling.ResultSuccess)
	}

	var payload struct {
		Items []readtools.DocumentHit `json:"items"`
	}
	if err := json.Unmarshal(result.Data, &payload); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	if len(payload.Items) != 1 || payload.Items[0].ID != "doc-a" {
		t.Fatalf("tenant-isolated tool result = %+v", payload.Items)
	}
}

func TestExecutorDoesNotExecuteDeprecatedToolVersion(t *testing.T) {
	definition := runnerTestDefinition()
	definition.Deprecated = true
	tool := &runnerTestTool{definition: definition}
	toolRegistry := registry.NewToolRegistry()
	if err := toolRegistry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	executor, err := NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}

	request := validToolCallRequest(time.Now().Add(time.Hour))
	request.CapabilityToken.Permissions = []tooling.Permission{tooling.Permission("docs:read")}

	if _, err := executor.Execute(context.Background(), request); !errors.Is(err, registry.ErrToolDeprecated) {
		t.Fatalf("Execute() error = %v, want ErrToolDeprecated", err)
	}
	if tool.executeCalls != 0 {
		t.Fatalf("Execute() calls = %d, want 0", tool.executeCalls)
	}
}

type runnerTestTool struct {
	definition   tooling.ToolDefinition
	executeCalls int
	executeErr   error
}

func (t *runnerTestTool) Definition() tooling.ToolDefinition {
	return t.definition
}

func (t *runnerTestTool) Execute(context.Context, tooling.ToolExecutionRequest) (tooling.ToolResult, error) {
	t.executeCalls++
	if t.executeErr != nil {
		return tooling.ToolResult{}, t.executeErr
	}
	return tooling.ToolResult{
		ToolName:    t.definition.Name,
		ToolVersion: t.definition.Version,
		Status:      tooling.ResultSuccess,
		Data:        json.RawMessage(`{"ok":true}`),
	}, nil
}

func runnerTestDefinition() tooling.ToolDefinition {
	return tooling.ToolDefinition{
		Name:            "search_project_docs",
		Version:         "v1",
		Description:     "搜索项目文档",
		Permission:      "docs:read",
		RiskLevel:       tooling.RiskLow,
		EffectClass:     tooling.EffectRead,
		Timeout:         time.Second,
		ParameterSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string"}},"additionalProperties":false}`),
		TenantScope:     tooling.TenantScopeCurrentTenant,
		CapabilityTags:  []string{"docs"},
	}
}

func TestRunnerErrorsAreStableSentinels(t *testing.T) {
	errs := []error{
		ErrExecutorNotConfigured,
		ErrInvalidToolCallRequest,
		ErrInvalidToolDecision,
		ErrToolVersionRequired,
		ErrArgumentsEncodingFailed,
	}

	for _, err := range errs {
		if err == nil {
			t.Fatal("runner error must not be nil")
		}
		if !errors.Is(err, err) {
			t.Fatalf("errors.Is(%v, %v) = false", err, err)
		}
	}
}
