package readtools

import (
	"context"
	"sort"
	"strings"
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

// ProjectDocument 是内存文档数据源使用的租户和项目归属信息。
type ProjectDocument struct {
	TenantID  string
	ProjectID string
	Hit       DocumentHit
}

// MemoryProjectDocStore 是只读的内存文档数据源，供测试和本地演示使用。
//
// 构造完成后数据不会再变化，因此查询时不需要锁，也不会产生跨租户写入问题。
type MemoryProjectDocStore struct {
	documents []ProjectDocument
}

// NewMemoryProjectDocStore 创建内存文档数据源。
func NewMemoryProjectDocStore(documents []ProjectDocument) *MemoryProjectDocStore {
	cloned := make([]ProjectDocument, len(documents))
	copy(cloned, documents)
	sort.SliceStable(cloned, func(i, j int) bool {
		if cloned[i].TenantID != cloned[j].TenantID {
			return cloned[i].TenantID < cloned[j].TenantID
		}
		if cloned[i].ProjectID != cloned[j].ProjectID {
			return cloned[i].ProjectID < cloned[j].ProjectID
		}
		return cloned[i].Hit.ID < cloned[j].Hit.ID
	})
	return &MemoryProjectDocStore{documents: cloned}
}

// Search 按租户、项目和关键词执行确定性的大小写不敏感搜索。
func (s *MemoryProjectDocStore) Search(
	ctx context.Context,
	tenantID string,
	projectID string,
	query string,
	limit int,
) ([]DocumentHit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	tenantID = strings.TrimSpace(tenantID)
	projectID = strings.TrimSpace(projectID)
	query = strings.ToLower(strings.TrimSpace(query))
	projectExists := false
	hits := make([]DocumentHit, 0, limit)

	for _, document := range s.documents {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if document.TenantID != tenantID || document.ProjectID != projectID {
			continue
		}
		projectExists = true
		if !matchesDocument(document.Hit, query) {
			continue
		}
		hits = append(hits, document.Hit)
		if len(hits) == limit {
			break
		}
	}
	if !projectExists {
		return nil, ErrProjectNotFound
	}
	return hits, nil
}

func matchesDocument(hit DocumentHit, query string) bool {
	fields := []string{hit.ID, hit.Title, hit.Summary, hit.Source, hit.Location}
	for _, field := range fields {
		if strings.Contains(strings.ToLower(field), query) {
			return true
		}
	}
	return false
}

var _ ProjectDocStore = (*MemoryProjectDocStore)(nil)
