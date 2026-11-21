package readtools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agent-project/internal/tool/tooling"
)

func TestSearchProjectDocsReturnsStableHitsAndSources(t *testing.T) {
	store := NewMemoryProjectDocStore([]ProjectDocument{
		{
			TenantID:  "tenant-a",
			ProjectID: "project-a",
			Hit: DocumentHit{
				ID:       "doc-2",
				Title:    "部署手册",
				Summary:  "服务部署说明",
				Source:   "repo",
				Location: "docs/deploy.md",
			},
		},
		{
			TenantID:  "tenant-a",
			ProjectID: "project-a",
			Hit: DocumentHit{
				ID:       "doc-1",
				Title:    "错误定位",
				Summary:  "订单服务错误排查",
				Source:   "wiki",
				Location: "orders/errors",
			},
		},
	})
	observer := &recordingObserver{}
	tool := NewSearchProjectDocsTool(store, observer)

	result, err := tooling.Invoke(
		context.Background(),
		tool,
		projectDocsRequest(`{"project_id":"project-a","query":"错误","limit":10}`),
	)
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if result.Status != tooling.ResultSuccess {
		t.Fatalf("result status = %q, want success", result.Status)
	}

	var payload searchProjectDocsResult
	if err := json.Unmarshal(result.Data, &payload); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(payload.Items) != 1 || payload.Items[0].ID != "doc-1" {
		t.Fatalf("unexpected search result: %+v", payload)
	}
	if len(result.Source) != 1 ||
		result.Source[0].Source != "wiki" ||
		result.Source[0].Location != "orders/errors" {
		t.Fatalf("unexpected sources: %+v", result.Source)
	}
	if len(observer.items) != 1 || observer.items[0].ResultStatus != tooling.ResultSuccess {
		t.Fatalf("unexpected observations: %+v", observer.items)
	}
}

func TestSearchProjectDocsAppliesLimitAndReturnsEmptyItems(t *testing.T) {
	store := NewMemoryProjectDocStore([]ProjectDocument{
		{TenantID: "tenant-a", ProjectID: "project-a", Hit: DocumentHit{ID: "doc-1", Summary: "error"}},
		{TenantID: "tenant-a", ProjectID: "project-a", Hit: DocumentHit{ID: "doc-2", Summary: "error"}},
	})
	tool := NewSearchProjectDocsTool(store, nil)

	result, err := tooling.Invoke(
		context.Background(),
		tool,
		projectDocsRequest(`{"project_id":"project-a","query":"error","limit":1}`),
	)
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	var limited searchProjectDocsResult
	if err := json.Unmarshal(result.Data, &limited); err != nil {
		t.Fatalf("decode limited result: %v", err)
	}
	if len(limited.Items) != 1 || limited.Items[0].ID != "doc-1" {
		t.Fatalf("unexpected limited result: %+v", limited)
	}

	result, err = tooling.Invoke(
		context.Background(),
		tool,
		projectDocsRequest(`{"project_id":"project-a","query":"missing","limit":10}`),
	)
	if err != nil {
		t.Fatalf("empty search error = %v", err)
	}
	var empty searchProjectDocsResult
	if err := json.Unmarshal(result.Data, &empty); err != nil {
		t.Fatalf("decode empty result: %v", err)
	}
	if result.Status != tooling.ResultSuccess || len(empty.Items) != 0 {
		t.Fatalf("unexpected empty result: status=%q payload=%+v", result.Status, empty)
	}
}

func TestSearchProjectDocsMapsProjectNotFound(t *testing.T) {
	store := NewMemoryProjectDocStore([]ProjectDocument{
		{TenantID: "tenant-a", ProjectID: "project-a", Hit: DocumentHit{ID: "doc-1", Summary: "error"}},
	})
	tool := NewSearchProjectDocsTool(store, nil)
	request := projectDocsRequest(`{"project_id":"project-missing","query":"error","limit":10}`)
	request.ResourceScope = []string{"project:project-missing"}
	request.CapabilityToken.ResourceScope = []string{"project:project-missing"}

	result, err := tooling.Invoke(
		context.Background(),
		tool,
		request,
	)
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if result.Status != tooling.ResultNotFound ||
		result.Error == nil ||
		result.Error.Code != "project_not_found" {
		t.Fatalf("unexpected not found result: %+v", result)
	}
}

func TestSearchProjectDocsRejectsProjectOutsideResourceScope(t *testing.T) {
	store := NewMemoryProjectDocStore([]ProjectDocument{
		{TenantID: "tenant-a", ProjectID: "project-a", Hit: DocumentHit{ID: "doc-1", Summary: "error"}},
	})
	tool := NewSearchProjectDocsTool(store, nil)
	request := projectDocsRequest(`{"project_id":"project-a","query":"error","limit":10}`)
	request.ResourceScope = []string{"project:other"}
	request.CapabilityToken.ResourceScope = []string{"project:other"}

	result, err := tooling.Invoke(context.Background(), tool, request)
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if result.Status != tooling.ResultForbidden ||
		result.Error == nil ||
		result.Error.Code != "project_scope_denied" {
		t.Fatalf("unexpected result after project scope rejection: %+v", result)
	}
}

func TestSearchProjectDocsMapsDataSourceErrors(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		status    tooling.ResultStatus
		errorCode string
		retryable bool
	}{
		{
			name:      "timeout",
			err:       ErrDocumentStoreTimeout,
			status:    tooling.ResultTimeout,
			errorCode: "timeout",
			retryable: true,
		},
		{
			name:      "internal",
			err:       ErrDocumentStoreInternal,
			status:    tooling.ResultInternalError,
			errorCode: "document_search_failed",
			retryable: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tool := NewSearchProjectDocsTool(errorProjectDocStore{err: test.err}, nil)
			result, err := tooling.Invoke(
				context.Background(),
				tool,
				projectDocsRequest(`{"project_id":"project-a","query":"error","limit":10}`),
			)
			if err != nil {
				t.Fatalf("Invoke() error = %v", err)
			}
			if result.Status != test.status ||
				result.Error == nil ||
				result.Error.Code != test.errorCode ||
				result.Error.Retryable != test.retryable {
				t.Fatalf("unexpected mapped result: %+v", result)
			}
		})
	}
}

func TestSearchProjectDocsContextCancellationStopsResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := cancelingProjectDocStore{cancel: cancel}
	tool := NewSearchProjectDocsTool(store, nil)

	_, err := tooling.Invoke(
		ctx,
		tool,
		projectDocsRequest(`{"project_id":"project-a","query":"error","limit":10}`),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Invoke() error = %v, want context.Canceled", err)
	}
}

func TestMemoryProjectDocStoreDoesNotCrossTenantData(t *testing.T) {
	store := NewMemoryProjectDocStore([]ProjectDocument{
		{TenantID: "tenant-a", ProjectID: "project-a", Hit: DocumentHit{ID: "doc-a", Summary: "error"}},
		{TenantID: "tenant-b", ProjectID: "project-a", Hit: DocumentHit{ID: "doc-b", Summary: "error"}},
	})
	hits, err := store.Search(context.Background(), "tenant-a", "project-a", "error", 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(hits) != 1 || hits[0].ID != "doc-a" {
		t.Fatalf("cross-tenant result leakage: %+v", hits)
	}
}

type errorProjectDocStore struct {
	err error
}

func (s errorProjectDocStore) Search(context.Context, string, string, string, int) ([]DocumentHit, error) {
	return nil, s.err
}

type cancelingProjectDocStore struct {
	cancel context.CancelFunc
}

func (s cancelingProjectDocStore) Search(ctx context.Context, _ string, _ string, _ string, _ int) ([]DocumentHit, error) {
	s.cancel()
	return nil, ctx.Err()
}

func projectDocsRequest(arguments string) tooling.ToolExecutionRequest {
	return tooling.ToolExecutionRequest{
		ToolName:      SearchProjectDocsName,
		ToolVersion:   ReadToolVersion,
		TenantID:      "tenant-a",
		RunID:         "run-1",
		RequestID:     "req-docs-1",
		Arguments:     json.RawMessage(arguments),
		ResourceScope: []string{"project:project-a"},
		CapabilityToken: tooling.CapabilityToken{
			TokenID:       "token-docs-1",
			TenantID:      "tenant-a",
			RunID:         "run-1",
			ToolName:      SearchProjectDocsName,
			ToolVersion:   ReadToolVersion,
			ResourceScope: []string{"project:project-a"},
			Permissions:   []tooling.Permission{"project_docs:read"},
			ExpiresAt:     time.Now().Add(time.Hour),
			Nonce:         "nonce-docs-1",
		},
	}
}
