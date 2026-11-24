package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-project/internal/tool/tooling"
)

func TestNewRegistryInitializesEmptyRegistry(t *testing.T) {
	registry := NewToolRegistry()
	if registry == nil {
		t.Fatal("NewToolRegistry() returned nil")
	}
	if registry.versions == nil {
		t.Fatal("registry versions map is nil")
	}
	if len(registry.versions) != 0 {
		t.Fatalf("registry versions = %d, want 0", len(registry.versions))
	}
}

func TestRegistryErrorsAreStableSentinels(t *testing.T) {
	errs := []error{
		ErrInvalidRegistryTool,
		ErrDuplicateTool,
		ErrToolNotFound,
		ErrToolVersionNotFound,
		ErrToolDeprecated,
	}

	for _, err := range errs {
		if err == nil {
			t.Fatal("registry error must not be nil")
		}
		if !errors.Is(err, err) {
			t.Fatalf("errors.Is(%v, %v) = false", err, err)
		}
	}
}

func TestRegisteredToolStoresDefinitionAndTool(t *testing.T) {
	definition := tooling.ToolDefinition{
		Name:    "example",
		Version: "v1",
	}
	registered := RegisteredTool{
		Definition: definition,
	}

	if registered.Definition.Name != "example" ||
		registered.Definition.Version != "v1" {
		t.Fatalf("registered definition = %+v", registered.Definition)
	}
	if registered.Tool != nil {
		t.Fatal("registered tool should be nil when no tool instance is assigned")
	}
}

func TestCloneToolDefinitionCopiesMutableFields(t *testing.T) {
	original := tooling.ToolDefinition{
		Name:            "search_project_docs",
		Version:         "v1",
		ParameterSchema: []byte(`{"type":"object"}`),
		AllowedTenants:  []string{"tenant-a", "tenant-b"},
		CapabilityTags:  []string{"read_only", "project_docs"},
	}

	cloned := cloneToolDefinition(original)

	original.ParameterSchema[0] = '['
	original.AllowedTenants[0] = "changed-tenant"
	original.CapabilityTags[0] = "changed-capability"

	if bytes.Equal(cloned.ParameterSchema, original.ParameterSchema) {
		t.Fatal("cloned parameter schema shares the original backing array")
	}
	if cloned.AllowedTenants[0] != "tenant-a" {
		t.Fatalf("cloned allowed tenants = %v, want original value", cloned.AllowedTenants)
	}
	if cloned.CapabilityTags[0] != "read_only" {
		t.Fatalf("cloned capability tags = %v, want original value", cloned.CapabilityTags)
	}

	cloned.ParameterSchema[1] = 'X'
	cloned.AllowedTenants[1] = "clone-tenant"
	cloned.CapabilityTags[1] = "clone-capability"

	if original.AllowedTenants[1] != "tenant-b" {
		t.Fatalf("original allowed tenants changed through clone: %v", original.AllowedTenants)
	}
	if original.CapabilityTags[1] != "project_docs" {
		t.Fatalf("original capability tags changed through clone: %v", original.CapabilityTags)
	}
}

func TestCloneToolDefinitionPreservesNilSlices(t *testing.T) {
	cloned := cloneToolDefinition(tooling.ToolDefinition{})
	if cloned.ParameterSchema != nil ||
		cloned.AllowedTenants != nil ||
		cloned.CapabilityTags != nil {
		t.Fatal("nil mutable fields should remain nil after cloning")
	}
}

func TestRegistryRegisterValidTool(t *testing.T) {
	registry := NewToolRegistry()
	tool := &testRegistryTool{
		definition: testRegistryDefinition("example", "v1"),
	}

	if err := registry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	versions := registry.versions["example"]
	if len(versions) != 1 {
		t.Fatalf("registered versions = %d, want 1", len(versions))
	}
	registered, exists := versions["v1"]
	if !exists {
		t.Fatal("registered tool version v1 not found")
	}
	if registered.Tool != tool {
		t.Fatal("registered tool instance does not match input")
	}
}

func TestRegistryRegisterRejectsInvalidTools(t *testing.T) {
	tests := []struct {
		name string
		tool tooling.Tool
	}{
		{name: "nil tool", tool: nil},
		{name: "invalid definition", tool: &testRegistryTool{}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := NewToolRegistry()
			if err := registry.Register(test.tool); err != ErrInvalidRegistryTool {
				t.Fatalf("Register() error = %v, want ErrInvalidRegistryTool", err)
			}
		})
	}
}

func TestRegistryRegisterRejectsDuplicateVersion(t *testing.T) {
	registry := NewToolRegistry()
	first := &testRegistryTool{definition: testRegistryDefinition("example", "v1")}
	second := &testRegistryTool{definition: testRegistryDefinition("example", "v1")}

	if err := registry.Register(first); err != nil {
		t.Fatalf("first Register() error = %v", err)
	}
	if err := registry.Register(second); err != ErrDuplicateTool {
		t.Fatalf("duplicate Register() error = %v, want ErrDuplicateTool", err)
	}
}

func TestRegistryRegisterAllowsMultipleVersions(t *testing.T) {
	registry := NewToolRegistry()
	for _, version := range []string{"v1", "v2"} {
		if err := registry.Register(&testRegistryTool{
			definition: testRegistryDefinition("example", version),
		}); err != nil {
			t.Fatalf("Register(%s) error = %v", version, err)
		}
	}

	if got := len(registry.versions["example"]); got != 2 {
		t.Fatalf("registered versions = %d, want 2", got)
	}
}

func TestRegistryRegisterNormalizesToolIdentity(t *testing.T) {
	registry := NewToolRegistry()
	tool := &testRegistryTool{
		definition: testRegistryDefinition(" example ", " v1 "),
	}

	if err := registry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	registered, err := registry.Resolve("example", "v1")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if registered.Definition.Name != "example" ||
		registered.Definition.Version != "v1" {
		t.Fatalf("normalized definition = %+v", registered.Definition)
	}
	if registered.Tool.Definition().Name != "example" ||
		registered.Tool.Definition().Version != "v1" {
		t.Fatalf("normalized executable definition = %+v", registered.Tool.Definition())
	}

	request := tooling.ToolExecutionRequest{
		ToolName:    "example",
		ToolVersion: "v1",
		TenantID:    "tenant-a",
		RunID:       "run-1",
		RequestID:   "request-1",
		Arguments:   json.RawMessage(`{}`),
		CapabilityToken: tooling.CapabilityToken{
			TokenID:     "cap-1",
			TenantID:    "tenant-a",
			RunID:       "run-1",
			ToolName:    "example",
			ToolVersion: "v1",
			Permissions: []tooling.Permission{"test:read"},
			ExpiresAt:   time.Now().Add(time.Hour),
			Nonce:       "nonce-1",
		},
	}
	result, err := tooling.Invoke(context.Background(), registered.Tool, request)
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if result.ToolName != "example" || result.ToolVersion != "v1" {
		t.Fatalf("normalized result identity = %+v", result)
	}
}

func TestRegistryResolveReturnsRegisteredTool(t *testing.T) {
	registry := NewToolRegistry()
	tool := &testRegistryTool{
		definition: testRegistryDefinition("example", "v1"),
	}
	if err := registry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	registered, err := registry.Resolve(" example ", " v1 ")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if registered.Tool != tool {
		t.Fatal("Resolve() returned a different tool instance")
	}
	if registered.Definition.Name != "example" ||
		registered.Definition.Version != "v1" {
		t.Fatalf("Resolve() definition = %+v", registered.Definition)
	}
}

func TestRegistryResolveDistinguishesMissingToolAndVersion(t *testing.T) {
	registry := NewToolRegistry()
	if err := registry.Register(&testRegistryTool{
		definition: testRegistryDefinition("example", "v1"),
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if _, err := registry.Resolve("missing", "v1"); !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("missing tool error = %v, want ErrToolNotFound", err)
	}
	if _, err := registry.Resolve("example", "missing"); !errors.Is(err, ErrToolVersionNotFound) {
		t.Fatalf("missing version error = %v, want ErrToolVersionNotFound", err)
	}
}

func TestRegistryResolveRejectsDeprecatedVersion(t *testing.T) {
	registry := NewToolRegistry()
	definition := testRegistryDefinition("example", "v1")
	definition.Deprecated = true
	if err := registry.Register(&testRegistryTool{definition: definition}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if _, err := registry.Resolve("example", "v1"); !errors.Is(err, ErrToolDeprecated) {
		t.Fatalf("deprecated version error = %v, want ErrToolDeprecated", err)
	}
}

func TestRegistryResolveReturnsDefinitionCopy(t *testing.T) {
	registry := NewToolRegistry()
	definition := testRegistryDefinition("example", "v1")
	definition.CapabilityTags = []string{"read_only"}
	if err := registry.Register(&testRegistryTool{definition: definition}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	registered, err := registry.Resolve("example", "v1")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	registered.Definition.ParameterSchema[0] = '['
	registered.Definition.CapabilityTags[0] = "changed"

	again, err := registry.Resolve("example", "v1")
	if err != nil {
		t.Fatalf("second Resolve() error = %v", err)
	}
	if string(again.Definition.ParameterSchema) != `{"type":"object"}` {
		t.Fatalf("stored schema was modified: %s", again.Definition.ParameterSchema)
	}
	if again.Definition.CapabilityTags[0] != "read_only" {
		t.Fatalf("stored capability tags were modified: %v", again.Definition.CapabilityTags)
	}
}

func TestRegistryListAllIncludesDeprecatedToolsAndSortsResults(t *testing.T) {
	registry := NewToolRegistry()
	definitions := []struct {
		name       string
		version    string
		deprecated bool
		tags       []string
	}{
		{name: "search", version: "v2", tags: []string{"write"}},
		{name: "search", version: "v1", deprecated: true, tags: []string{"read"}},
		{name: "current_time", version: "v1", tags: []string{"time"}},
	}

	for _, item := range definitions {
		definition := testRegistryDefinition(item.name, item.version)
		definition.Deprecated = item.deprecated
		definition.CapabilityTags = item.tags
		if err := registry.Register(&testRegistryTool{definition: definition}); err != nil {
			t.Fatalf("Register(%s/%s) error = %v", item.name, item.version, err)
		}
	}

	got := registry.ListAll()
	if len(got) != 3 {
		t.Fatalf("ListAll() returned %d definitions, want 3", len(got))
	}
	if got[0].Name != "current_time" ||
		got[1].Name != "search" || got[1].Version != "v1" ||
		got[2].Name != "search" || got[2].Version != "v2" {
		t.Fatalf("ListAll() order = %s/%s, %s/%s, %s/%s",
			got[0].Name, got[0].Version,
			got[1].Name, got[1].Version,
			got[2].Name, got[2].Version,
		)
	}
	if !got[1].Deprecated {
		t.Fatal("ListAll() should include deprecated definitions")
	}
}

func TestRegistryListAllReturnsDefinitionCopies(t *testing.T) {
	registry := NewToolRegistry()
	definition := testRegistryDefinition("example", "v1")
	definition.CapabilityTags = []string{"read_only"}
	if err := registry.Register(&testRegistryTool{definition: definition}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	got := registry.ListAll()
	got[0].ParameterSchema[0] = '['
	got[0].CapabilityTags[0] = "changed"

	again := registry.ListAll()
	if string(again[0].ParameterSchema) != `{"type":"object"}` {
		t.Fatalf("stored schema was modified: %s", again[0].ParameterSchema)
	}
	if again[0].CapabilityTags[0] != "read_only" {
		t.Fatalf("stored capability tags were modified: %v", again[0].CapabilityTags)
	}
}

func TestRegistryListAvailableExcludesDeprecatedToolsAndSortsResults(t *testing.T) {
	registry := NewToolRegistry()
	definitions := []struct {
		name       string
		version    string
		deprecated bool
		tags       []string
	}{
		{name: "search", version: "v2", tags: []string{"write"}},
		{name: "search", version: "v1", deprecated: true, tags: []string{"read"}},
		{name: "current_time", version: "v1", tags: []string{"time"}},
	}

	for _, item := range definitions {
		definition := testRegistryDefinition(item.name, item.version)
		definition.Deprecated = item.deprecated
		definition.CapabilityTags = item.tags
		if err := registry.Register(&testRegistryTool{definition: definition}); err != nil {
			t.Fatalf("Register(%s/%s) error = %v", item.name, item.version, err)
		}
	}

	got := registry.ListAvailable()
	if len(got) != 2 {
		t.Fatalf("ListAvailable() returned %d definitions, want 2", len(got))
	}
	if got[0].Name != "current_time" ||
		got[1].Name != "search" || got[1].Version != "v2" {
		t.Fatalf("ListAvailable() order = %s/%s, %s/%s",
			got[0].Name, got[0].Version,
			got[1].Name, got[1].Version,
		)
	}
}

func TestRegistryListAvailableReturnsDefinitionCopies(t *testing.T) {
	registry := NewToolRegistry()
	definition := testRegistryDefinition("example", "v1")
	definition.CapabilityTags = []string{"read_only"}
	if err := registry.Register(&testRegistryTool{definition: definition}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	got := registry.ListAvailable()
	got[0].ParameterSchema[0] = '['
	got[0].CapabilityTags[0] = "changed"

	again := registry.ListAvailable()
	if string(again[0].ParameterSchema) != `{"type":"object"}` {
		t.Fatalf("stored schema was modified: %s", again[0].ParameterSchema)
	}
	if again[0].CapabilityTags[0] != "read_only" {
		t.Fatalf("stored capability tags were modified: %v", again[0].CapabilityTags)
	}
}

func TestRegistryFindByCapabilityFiltersDeprecatedToolsAndSortsResults(t *testing.T) {
	registry := NewToolRegistry()
	definitions := []struct {
		name       string
		version    string
		deprecated bool
		tags       []string
	}{
		{name: "search", version: "v2", tags: []string{"read", "docs"}},
		{name: "search", version: "v1", deprecated: true, tags: []string{"read"}},
		{name: "current_time", version: "v1", tags: []string{"read", "time"}},
		{name: "write", version: "v1", tags: []string{"write"}},
	}

	for _, item := range definitions {
		definition := testRegistryDefinition(item.name, item.version)
		definition.Deprecated = item.deprecated
		definition.CapabilityTags = item.tags
		if err := registry.Register(&testRegistryTool{definition: definition}); err != nil {
			t.Fatalf("Register(%s/%s) error = %v", item.name, item.version, err)
		}
	}

	got := registry.FindByCapability(" read ")
	if len(got) != 2 {
		t.Fatalf("FindByCapability() returned %d definitions, want 2", len(got))
	}
	if got[0].Name != "current_time" ||
		got[1].Name != "search" || got[1].Version != "v2" {
		t.Fatalf("FindByCapability() order = %s/%s, %s/%s",
			got[0].Name, got[0].Version,
			got[1].Name, got[1].Version,
		)
	}
}

func TestRegistryFindByCapabilityReturnsEmptyForBlankTag(t *testing.T) {
	registry := NewToolRegistry()
	if err := registry.Register(&testRegistryTool{
		definition: testRegistryDefinition("example", "v1"),
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if got := registry.FindByCapability("   "); len(got) != 0 {
		t.Fatalf("FindByCapability() with blank tag returned %d definitions, want 0", len(got))
	}
}

func TestRegistryFindByCapabilityReturnsDefinitionCopies(t *testing.T) {
	registry := NewToolRegistry()
	definition := testRegistryDefinition("example", "v1")
	definition.CapabilityTags = []string{"read_only"}
	if err := registry.Register(&testRegistryTool{definition: definition}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	got := registry.FindByCapability("read_only")
	got[0].ParameterSchema[0] = '['
	got[0].CapabilityTags[0] = "changed"

	again := registry.FindByCapability("read_only")
	if string(again[0].ParameterSchema) != `{"type":"object"}` {
		t.Fatalf("stored schema was modified: %s", again[0].ParameterSchema)
	}
	if again[0].CapabilityTags[0] != "read_only" {
		t.Fatalf("stored capability tags were modified: %v", again[0].CapabilityTags)
	}
}

func TestRegistryVersionsReturnsSortedVersions(t *testing.T) {
	registry := NewToolRegistry()
	for _, version := range []string{"v10", "v2", "v1"} {
		if err := registry.Register(&testRegistryTool{
			definition: testRegistryDefinition("example", version),
		}); err != nil {
			t.Fatalf("Register(%s) error = %v", version, err)
		}
	}

	got := registry.Versions(" example ")
	want := []string{"v1", "v10", "v2"}
	if len(got) != len(want) {
		t.Fatalf("Versions() returned %d versions, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Versions() = %v, want %v", got, want)
		}
	}
}

func TestRegistryVersionsReturnsEmptyForUnknownTool(t *testing.T) {
	registry := NewToolRegistry()
	if got := registry.Versions("missing"); len(got) != 0 {
		t.Fatalf("Versions() for unknown tool returned %v, want empty", got)
	}
	if got := registry.Versions("   "); len(got) != 0 {
		t.Fatalf("Versions() for blank tool returned %v, want empty", got)
	}
}

func TestRegistryResolveFeedsToolingInvokeWithoutBypassingBoundary(t *testing.T) {
	var calls atomic.Int32
	registry := NewToolRegistry()
	tool := &testRegistryTool{
		definition: testRegistryDefinition("example", "v1"),
		calls:      &calls,
	}
	if err := registry.Register(tool); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("Register() must not execute the tool")
	}

	registered, err := registry.Resolve("example", "v1")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("Resolve() must not execute the tool")
	}

	definition := registered.Definition
	request := tooling.ToolExecutionRequest{
		ToolName:    definition.Name,
		ToolVersion: definition.Version,
		TenantID:    "tenant-a",
		RunID:       "run-1",
		RequestID:   "request-1",
		Arguments:   json.RawMessage(`{}`),
		CapabilityToken: tooling.CapabilityToken{
			TokenID:     "cap-1",
			TenantID:    "tenant-a",
			RunID:       "run-1",
			ToolName:    definition.Name,
			ToolVersion: definition.Version,
			Permissions: []tooling.Permission{definition.Permission},
			ExpiresAt:   time.Now().Add(time.Hour),
			Nonce:       "nonce-1",
		},
	}
	result, err := tooling.Invoke(context.Background(), registered.Tool, request)
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if result.Status != tooling.ResultSuccess {
		t.Fatalf("Invoke() result status = %s, want %s", result.Status, tooling.ResultSuccess)
	}
	if calls.Load() != 1 {
		t.Fatalf("tool Execute calls = %d, want 1", calls.Load())
	}
}

func TestRegistrySupportsConcurrentRegistrationAndQueries(t *testing.T) {
	registry := NewToolRegistry()
	const toolCount = 32

	var workers sync.WaitGroup
	workers.Add(toolCount)
	for i := 0; i < toolCount; i++ {
		i := i
		go func() {
			defer workers.Done()

			version := fmt.Sprintf("v%02d", i)
			if err := registry.Register(&testRegistryTool{
				definition: testRegistryDefinition("concurrent", version),
			}); err != nil {
				t.Errorf("Register(%s) error = %v", version, err)
			}

			_, _ = registry.Resolve("concurrent", version)
			_ = registry.ListAll()
			_ = registry.ListAvailable()
			_ = registry.FindByCapability("test")
			_ = registry.Versions("concurrent")
		}()
	}
	workers.Wait()

	versions := registry.Versions("concurrent")
	if len(versions) != toolCount {
		t.Fatalf("Versions() returned %d versions, want %d", len(versions), toolCount)
	}
}

type testRegistryTool struct {
	definition tooling.ToolDefinition
	calls      *atomic.Int32
}

func (t *testRegistryTool) Definition() tooling.ToolDefinition {
	return t.definition
}

func (t *testRegistryTool) Execute(
	context.Context,
	tooling.ToolExecutionRequest,
) (tooling.ToolResult, error) {
	if t.calls != nil {
		t.calls.Add(1)
	}
	return tooling.ToolResult{
		ToolName:    t.definition.Name,
		ToolVersion: t.definition.Version,
		Status:      tooling.ResultSuccess,
		Data:        json.RawMessage(`{}`),
	}, nil
}

func testRegistryDefinition(name, version string) tooling.ToolDefinition {
	return tooling.ToolDefinition{
		Name:            name,
		Version:         version,
		Description:     "test tool",
		Permission:      tooling.Permission("test:read"),
		RiskLevel:       tooling.RiskLow,
		EffectClass:     tooling.EffectRead,
		Timeout:         time.Second,
		ParameterSchema: json.RawMessage(`{"type":"object"}`),
		TenantScope:     tooling.TenantScopeGlobal,
		CapabilityTags:  []string{"test"},
	}
}
