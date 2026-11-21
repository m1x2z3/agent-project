package tooling

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestToolDefinitionValidate(t *testing.T) {
	valid := testDefinition()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid definition rejected: %v", err)
	}

	tests := []struct {
		name string
		edit func(*ToolDefinition)
	}{
		{"missing name", func(d *ToolDefinition) { d.Name = "" }},
		{"missing version", func(d *ToolDefinition) { d.Version = "" }},
		{"missing permission", func(d *ToolDefinition) { d.Permission = "" }},
		{"invalid risk", func(d *ToolDefinition) { d.RiskLevel = "critical" }},
		{"invalid effect", func(d *ToolDefinition) { d.EffectClass = "unknown" }},
		{"invalid tenant scope", func(d *ToolDefinition) { d.TenantScope = "unknown" }},
		{"invalid timeout", func(d *ToolDefinition) { d.Timeout = 0 }},
		{"empty schema", func(d *ToolDefinition) { d.ParameterSchema = nil }},
		{"invalid schema JSON", func(d *ToolDefinition) { d.ParameterSchema = json.RawMessage(`{"type":`) }},
		{"schema is array", func(d *ToolDefinition) { d.ParameterSchema = json.RawMessage(`[]`) }},
		{"schema has invalid type", func(d *ToolDefinition) {
			d.ParameterSchema = json.RawMessage(`{"type":"not-a-json-schema-type"}`)
		}},
		{"allowlist without tenants", func(d *ToolDefinition) {
			d.TenantScope = TenantScopeAllowlist
			d.AllowedTenants = nil
		}},
		{"tenants on global scope", func(d *ToolDefinition) {
			d.AllowedTenants = []string{"tenant-a"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition := valid
			test.edit(&definition)
			if !errors.Is(definition.Validate(), ErrInvalidDefinition) {
				t.Fatalf("Validate() = %v, want ErrInvalidDefinition", definition.Validate())
			}
		})
	}
}

func TestToolExecutionRequestAndResultValidation(t *testing.T) {
	request := testRequest()
	if err := request.Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	request.Arguments = json.RawMessage(`not-json`)
	if !errors.Is(request.Validate(), ErrInvalidRequest) {
		t.Fatalf("invalid arguments accepted: %v", request.Validate())
	}

	result := ToolResult{
		ToolName:    "search_project_docs",
		ToolVersion: "v1",
		Status:      ResultSuccess,
		Data:        json.RawMessage(`{"items":[]}`),
	}
	if err := result.Validate(); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}
	result.Status = ResultTimeout
	if !errors.Is(result.Validate(), ErrResultInvalid) {
		t.Fatalf("timeout without error accepted: %v", result.Validate())
	}
}

func TestCapabilityTokenValidation(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	token := testToken(now.Add(time.Hour))
	if err := token.Validate(now); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	expired := token
	expired.ExpiresAt = now
	if !errors.Is(expired.Validate(now), ErrCapabilityExpired) {
		t.Fatalf("expired token error = %v", expired.Validate(now))
	}
	invalid := token
	invalid.Nonce = ""
	if !errors.Is(invalid.Validate(now), ErrCapabilityTokenInvalid) {
		t.Fatalf("invalid token error = %v", invalid.Validate(now))
	}
}

func TestValidateExecutionRequestBoundaries(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	definition := testDefinition()
	request := testRequest()
	request.CapabilityToken = testToken(now.Add(time.Hour))

	tests := []struct {
		name string
		edit func(*ToolDefinition, *ToolExecutionRequest)
		want error
	}{
		{"tenant mismatch", func(_ *ToolDefinition, r *ToolExecutionRequest) { r.TenantID = "tenant-b" }, ErrTenantMismatch},
		{"run mismatch", func(_ *ToolDefinition, r *ToolExecutionRequest) { r.RunID = "run-b" }, ErrRunMismatch},
		{"tool mismatch", func(_ *ToolDefinition, r *ToolExecutionRequest) { r.ToolName = "other" }, ErrToolMismatch},
		{"version mismatch", func(_ *ToolDefinition, r *ToolExecutionRequest) { r.ToolVersion = "v2" }, ErrToolVersionMismatch},
		{"expired token", func(_ *ToolDefinition, r *ToolExecutionRequest) {
			r.CapabilityToken.ExpiresAt = now
		}, ErrCapabilityExpired},
		{"missing permission", func(_ *ToolDefinition, r *ToolExecutionRequest) {
			r.CapabilityToken.Permissions = nil
		}, ErrPermissionDenied},
		{"resource denied", func(_ *ToolDefinition, r *ToolExecutionRequest) {
			r.ResourceScope = []string{"issue-2"}
		}, ErrResourceScopeDenied},
		{"arguments rejected by schema", func(_ *ToolDefinition, r *ToolExecutionRequest) {
			r.Arguments = json.RawMessage(`{"unexpected":true}`)
		}, ErrArgumentsInvalid},
		{"allowlist denied", func(d *ToolDefinition, _ *ToolExecutionRequest) {
			d.TenantScope = TenantScopeAllowlist
			d.AllowedTenants = []string{"tenant-b"}
		}, ErrTenantMismatch},
		{"current tenant requires resource", func(d *ToolDefinition, r *ToolExecutionRequest) {
			d.TenantScope = TenantScopeCurrentTenant
			r.ResourceScope = nil
		}, ErrResourceScopeDenied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotDefinition := definition
			gotRequest := cloneRequest(request)
			test.edit(&gotDefinition, &gotRequest)
			if !errors.Is(ValidateExecutionRequest(gotDefinition, gotRequest, now), test.want) {
				t.Fatalf("ValidateExecutionRequest() = %v, want %v", ValidateExecutionRequest(gotDefinition, gotRequest, now), test.want)
			}
		})
	}
}

func TestInvokeEnforcesEffectBoundary(t *testing.T) {
	var calls atomic.Int32
	tool := fakeTool{definition: testDefinition(), calls: &calls}
	result, err := Invoke(context.Background(), &tool, testRequest())
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if result.Status != ResultSuccess || calls.Load() != 1 {
		t.Fatalf("result=%+v calls=%d", result, calls.Load())
	}

	invalid := testRequest()
	invalid.CapabilityToken = CapabilityToken{}
	if _, err := Invoke(context.Background(), &tool, invalid); !errors.Is(err, ErrCapabilityTokenMissing) {
		t.Fatalf("missing token error = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("tool executed after invalid request: calls=%d", calls.Load())
	}
}

func TestHighRiskWriteWithoutCapabilityPermissionIsRejected(t *testing.T) {
	definition := testDefinition()
	definition.Name = "deploy_service"
	definition.Description = "部署服务"
	definition.Permission = "deploy:write"
	definition.RiskLevel = RiskHigh
	definition.EffectClass = EffectNonRetryableWrite
	request := testRequest()
	request.ToolName = definition.Name
	request.CapabilityToken.ToolName = definition.Name
	request.CapabilityToken.Permissions = nil

	if _, err := Invoke(context.Background(), &fakeTool{definition: definition}, request); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("missing permission error = %v, want ErrPermissionDenied", err)
	}

	request.CapabilityToken.Permissions = []Permission{definition.Permission}

	if _, err := Invoke(context.Background(), &fakeTool{definition: definition}, request); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("high-risk write error = %v, want ErrApprovalRequired", err)
	}
}

func TestInvokeStopsWhenContextAlreadyCanceled(t *testing.T) {
	var calls atomic.Int32
	tool := fakeTool{definition: testDefinition(), calls: &calls}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Invoke(ctx, &tool, testRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Invoke() error = %v, want context.Canceled", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("tool executed after context cancellation: calls=%d", calls.Load())
	}
}

func TestToolDecisionCannotCreateAuthorizedRequest(t *testing.T) {
	// ToolDecision 只有模型意图字段，没有 CapabilityToken；
	// 即使工具名和参数正确，缺少令牌也不能通过执行边界。
	request := testRequest()
	request.CapabilityToken = CapabilityToken{}
	if _, err := Invoke(context.Background(), &fakeTool{definition: testDefinition()}, request); !errors.Is(err, ErrCapabilityTokenMissing) {
		t.Fatalf("unauthorized model decision error = %v", err)
	}
}

func TestToolingErrorsDoNotEchoSensitiveInputs(t *testing.T) {
	const secret = "sk-test-secret"
	request := testRequest()
	request.Arguments = json.RawMessage(`{"prompt":"` + secret + `"}`)
	request.CapabilityToken = CapabilityToken{}
	err := func() error {
		_, err := Invoke(context.Background(), &fakeTool{definition: testDefinition()}, request)
		return err
	}()
	if err == nil {
		t.Fatal("expected invocation error")
	}
	if contains := err.Error(); contains == secret {
		t.Fatalf("error leaked sensitive input: %q", contains)
	}
}

type fakeTool struct {
	definition ToolDefinition
	calls      *atomic.Int32
}

func (t *fakeTool) Definition() ToolDefinition {
	return t.definition
}

func (t *fakeTool) Execute(context.Context, ToolExecutionRequest) (ToolResult, error) {
	if t.calls != nil {
		t.calls.Add(1)
	}
	return ToolResult{
		ToolName:    t.definition.Name,
		ToolVersion: t.definition.Version,
		Status:      ResultSuccess,
		Data:        json.RawMessage(`{"ok":true}`),
	}, nil
}

func testDefinition() ToolDefinition {
	return ToolDefinition{
		Name:            "search_project_docs",
		Version:         "v1",
		Description:     "搜索项目文档",
		Permission:      "docs:read",
		RiskLevel:       RiskLow,
		EffectClass:     EffectRead,
		Timeout:         2 * time.Second,
		ParameterSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"additionalProperties":false}`),
		TenantScope:     TenantScopeCurrentTenant,
		CapabilityTags:  []string{"read", "docs"},
	}
}

func testToken(expiresAt time.Time) CapabilityToken {
	return CapabilityToken{
		TokenID:       "cap-1",
		TenantID:      "tenant-a",
		RunID:         "run-1",
		ToolName:      "search_project_docs",
		ToolVersion:   "v1",
		ResourceScope: []string{"project-a"},
		Permissions:   []Permission{"docs:read"},
		ExpiresAt:     expiresAt,
		Nonce:         "nonce-1",
	}
}

func testRequest() ToolExecutionRequest {
	return ToolExecutionRequest{
		ToolName:        "search_project_docs",
		ToolVersion:     "v1",
		TenantID:        "tenant-a",
		RunID:           "run-1",
		RequestID:       "req-1",
		Arguments:       json.RawMessage(`{"query":"error"}`),
		ResourceScope:   []string{"project-a"},
		CapabilityToken: testToken(time.Now().Add(time.Hour)),
	}
}

func cloneRequest(request ToolExecutionRequest) ToolExecutionRequest {
	request.Arguments = append(json.RawMessage(nil), request.Arguments...)
	request.ResourceScope = append([]string(nil), request.ResourceScope...)
	request.CapabilityToken.ResourceScope = append([]string(nil), request.CapabilityToken.ResourceScope...)
	request.CapabilityToken.Permissions = append([]Permission(nil), request.CapabilityToken.Permissions...)
	return request
}
