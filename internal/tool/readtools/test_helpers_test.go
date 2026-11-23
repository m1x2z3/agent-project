package readtools

import (
	"context"
	"encoding/json"
	"time"

	"agent-project/internal/tool/tooling"
)

// recordingObserver 是多个工具测试共用的脱敏观测接收器。
type recordingObserver struct {
	items []ToolObservation
}

func (o *recordingObserver) Observe(observation ToolObservation) {
	o.items = append(o.items, observation)
}

func currentTimeRequest(arguments string) tooling.ToolExecutionRequest {
	return tooling.ToolExecutionRequest{
		ToolName:    GetCurrentTimeName,
		ToolVersion: ReadToolVersion,
		TenantID:    "tenant-a",
		RunID:       "run-1",
		RequestID:   "req-1",
		Arguments:   json.RawMessage(arguments),
		CapabilityToken: tooling.CapabilityToken{
			TokenID:     "token-1",
			TenantID:    "tenant-a",
			RunID:       "run-1",
			ToolName:    GetCurrentTimeName,
			ToolVersion: ReadToolVersion,
			Permissions: []tooling.Permission{"time:read"},
			ExpiresAt:   time.Now().Add(time.Hour),
			Nonce:       "nonce-1",
		},
	}
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

type cancelingProjectDocStore struct {
	cancel context.CancelFunc
}

func (s cancelingProjectDocStore) Search(ctx context.Context, _ string, _ string, _ string, _ int) ([]DocumentHit, error) {
	s.cancel()
	return nil, ctx.Err()
}
