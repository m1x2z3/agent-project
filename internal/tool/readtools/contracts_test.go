package readtools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agent-project/internal/tool/tooling"
)

func TestToolParameterSchemasAreValidReadOnlyContracts(t *testing.T) {
	tests := []struct {
		name   string
		schema func() json.RawMessage
		tool   string
	}{
		{
			name:   GetCurrentTimeName,
			schema: GetCurrentTimeParameterSchema,
			tool:   GetCurrentTimeName,
		},
		{
			name:   SearchProjectDocsName,
			schema: SearchProjectDocsParameterSchema,
			tool:   SearchProjectDocsName,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			schema := test.schema()
			if !json.Valid(schema) {
				t.Fatalf("schema is invalid JSON: %s", schema)
			}

			definition := tooling.ToolDefinition{
				Name:            test.tool,
				Version:         ReadToolVersion,
				Description:     "只读工具契约",
				Permission:      tooling.Permission("project:read"),
				RiskLevel:       tooling.RiskLow,
				EffectClass:     tooling.EffectRead,
				Timeout:         2 * time.Second,
				ParameterSchema: schema,
				TenantScope:     tooling.TenantScopeCurrentTenant,
				CapabilityTags:  []string{"read_only"},
			}
			if err := definition.Validate(); err != nil {
				t.Fatalf("tool definition rejected schema: %v", err)
			}
		})
	}
}

func TestReadToolResultContentKinds(t *testing.T) {
	documentTool := NewSearchProjectDocsTool(&fakeProjectDocStore{}, nil)
	if got := documentTool.Definition().ResultContentKind; got != tooling.ResultContentDocument {
		t.Fatalf("document tool result kind = %q, want %q", got, tooling.ResultContentDocument)
	}

	clockTool := NewCurrentTimeTool(nil, nil)
	if got := clockTool.Definition().ResultContentKind.Normalized(); got != tooling.ResultContentToolResult {
		t.Fatalf("clock tool result kind = %q, want %q", got, tooling.ResultContentToolResult)
	}
}

func TestToolParameterSchemaReturnsIndependentCopy(t *testing.T) {
	first := GetCurrentTimeParameterSchema()
	first[0] = 'x'

	second := GetCurrentTimeParameterSchema()
	if !json.Valid(second) {
		t.Fatalf("schema was mutated through returned slice: %s", second)
	}
}

func TestDocumentHitAndObserverContracts(t *testing.T) {
	var _ ProjectDocStore = (*fakeProjectDocStore)(nil)
	var _ Observer = NoopObserver{}

	hit := DocumentHit{
		ID:       "doc-1",
		Title:    "订单服务",
		Summary:  "订单服务错误定位说明",
		Source:   "repo",
		Location: "docs/orders.md#errors",
	}
	if hit.ID == "" || hit.Location == "" {
		t.Fatal("document hit contract should preserve identifying fields")
	}
}

type fakeProjectDocStore struct{}

func (*fakeProjectDocStore) Search(
	_ context.Context,
	_ string,
	_ string,
	_ string,
	_ int,
) ([]DocumentHit, error) {
	return nil, nil
}
