package configregistry

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Registry 是进程内的配置版本注册中心。
//
// 它模拟控制面保存发布版本和 Run 快照的核心语义，后续可以替换为
// PostgreSQL 持久化实现，而不改变调用方依赖的生命周期接口。
type Registry struct {
	mu       sync.RWMutex
	versions map[string]ConfigSnapshot
	active   string
	runs     map[string]RunSnapshot
}

// NewRegistry 创建一个空的配置注册中心。
func NewRegistry() *Registry {
	return &Registry{
		versions: make(map[string]ConfigSnapshot),
		runs:     make(map[string]RunSnapshot),
	}
}

// Publish 发布一个不可变版本，但不会自动切换当前 active 版本。
func (r *Registry) Publish(snapshot ConfigSnapshot) error {
	normalized, err := normalizeSnapshot(snapshot)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.versions[normalized.ReleaseVersion]; exists {
		return ErrVersionExists
	}
	r.versions[normalized.ReleaseVersion] = normalized
	return nil
}

// Activate 将已经发布的版本设置为当前 active 版本。
func (r *Registry) Activate(version string) error {
	version = strings.TrimSpace(version)
	if version == "" {
		return ErrVersionNotFound
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.versions[version]; !exists {
		return ErrVersionNotFound
	}
	// active 只是当前版本指针，不会修改或覆盖历史快照。
	r.active = version
	return nil
}

// Active 返回当前 active 版本的深拷贝。
func (r *Registry) Active() (ConfigSnapshot, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.active == "" {
		return ConfigSnapshot{}, ErrNoActiveVersion
	}
	return cloneSnapshot(r.versions[r.active]), nil
}

// Get 返回指定版本的深拷贝。
func (r *Registry) Get(version string) (ConfigSnapshot, error) {
	version = strings.TrimSpace(version)

	r.mu.RLock()
	defer r.mu.RUnlock()
	snapshot, exists := r.versions[version]
	if !exists {
		return ConfigSnapshot{}, ErrVersionNotFound
	}
	return cloneSnapshot(snapshot), nil
}

// History 返回按发布时间倒序排列的发布记录。
func (r *Registry) History() []ReleaseMetadata {
	r.mu.RLock()
	defer r.mu.RUnlock()

	history := make([]ReleaseMetadata, 0, len(r.versions))
	for _, snapshot := range r.versions {
		history = append(history, snapshot.Metadata)
	}
	sort.Slice(history, func(i, j int) bool {
		if history[i].PublishedAt.Equal(history[j].PublishedAt) {
			return history[i].Version > history[j].Version
		}
		return history[i].PublishedAt.After(history[j].PublishedAt)
	})
	return history
}

// Rollback 基于历史版本创建新的发布版本，并立即将新版本设为 active。
func (r *Registry) Rollback(
	targetVersion string,
	newVersion string,
	publishedBy string,
	effectiveAt time.Time,
) (ConfigSnapshot, error) {
	targetVersion = strings.TrimSpace(targetVersion)
	newVersion = strings.TrimSpace(newVersion)
	publishedBy = strings.TrimSpace(publishedBy)
	if targetVersion == "" || newVersion == "" || publishedBy == "" || effectiveAt.IsZero() {
		return ConfigSnapshot{}, ErrInvalidSnapshot
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.active == "" {
		return ConfigSnapshot{}, ErrNoActiveVersion
	}
	target, exists := r.versions[targetVersion]
	if !exists {
		return ConfigSnapshot{}, ErrVersionNotFound
	}
	if _, exists := r.versions[newVersion]; exists {
		return ConfigSnapshot{}, ErrVersionExists
	}

	rollback := cloneSnapshot(target)
	rollback.ReleaseVersion = newVersion
	rollback.Metadata = ReleaseMetadata{
		Version:         newVersion,
		PublishedBy:     publishedBy,
		PublishedAt:     time.Now().UTC(),
		EffectiveAt:     effectiveAt,
		PreviousVersion: r.active,
		RollbackFrom:    targetVersion,
	}
	r.versions[newVersion] = rollback
	r.active = newVersion
	return cloneSnapshot(rollback), nil
}

// BindRun 将 Run 绑定到调用时的 active 版本，并保存该版本的深拷贝。
func (r *Registry) BindRun(runID string) (RunSnapshot, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return RunSnapshot{}, ErrInvalidSnapshot
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bindRunLocked(runID, r.active)
}

// BindRunToVersion 将 Run 绑定到指定的已发布版本。
//
// 该方法用于灰度或回放场景：调用方先完成版本选择，再把本次 Run
// 固定到对应快照。绑定后即使 active 版本变化，Run 仍然读取原版本。
func (r *Registry) BindRunToVersion(runID string, releaseVersion string) (RunSnapshot, error) {
	runID = strings.TrimSpace(runID)
	releaseVersion = strings.TrimSpace(releaseVersion)
	if runID == "" {
		return RunSnapshot{}, ErrInvalidSnapshot
	}
	if releaseVersion == "" {
		return RunSnapshot{}, ErrVersionNotFound
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bindRunLocked(runID, releaseVersion)
}

func (r *Registry) bindRunLocked(runID string, releaseVersion string) (RunSnapshot, error) {
	// 调用方必须已经持有写锁。这里先把快照复制后存入 Registry，
	// 返回时再复制一份，分别保护注册中心内部状态和调用方拿到的结果。
	if _, exists := r.runs[runID]; exists {
		return RunSnapshot{}, ErrRunAlreadyBound
	}
	if releaseVersion == "" {
		return RunSnapshot{}, ErrNoActiveVersion
	}
	snapshot, exists := r.versions[releaseVersion]
	if !exists {
		return RunSnapshot{}, ErrVersionNotFound
	}

	bound := RunSnapshot{
		RunID:          runID,
		ReleaseVersion: releaseVersion,
		Snapshot:       cloneSnapshot(snapshot),
	}
	r.runs[runID] = bound
	return cloneRunSnapshot(bound), nil
}

// GetRunSnapshot 返回已绑定 Run 的深拷贝。
func (r *Registry) GetRunSnapshot(runID string) (RunSnapshot, error) {
	runID = strings.TrimSpace(runID)

	r.mu.RLock()
	defer r.mu.RUnlock()
	bound, exists := r.runs[runID]
	if !exists {
		return RunSnapshot{}, ErrRunNotFound
	}
	return cloneRunSnapshot(bound), nil
}

// ResolveVersionReferences 将快照中的 Prompt、Schema 和模型路由版本整理成
// 可传递给结构化输出解析层的版本引用，不直接依赖 structuredoutput 包。
func (s ConfigSnapshot) ResolveVersionReferences(
	promptName string,
	schemaKind string,
	task string,
) (VersionReferences, error) {
	prompt, promptOK := s.Prompts[strings.TrimSpace(promptName)]
	schema, schemaOK := s.Schemas[strings.TrimSpace(schemaKind)]
	route, routeOK := s.Routes[strings.TrimSpace(task)]
	if !promptOK || !schemaOK || !routeOK {
		return VersionReferences{}, ErrVersionReferenceNotFound
	}
	return VersionReferences{
		Provider:           route.Provider,
		Model:              route.Model,
		PromptVersion:      prompt.Version,
		SchemaVersion:      schema.Version,
		ModelConfigVersion: route.Version,
	}, nil
}

func normalizeSnapshot(snapshot ConfigSnapshot) (ConfigSnapshot, error) {
	if err := validateSnapshot(snapshot); err != nil {
		return ConfigSnapshot{}, err
	}
	return cloneSnapshot(snapshot), nil
}

func validateSnapshot(snapshot ConfigSnapshot) error {
	if strings.TrimSpace(snapshot.ReleaseVersion) == "" ||
		strings.TrimSpace(snapshot.Metadata.Version) == "" ||
		snapshot.ReleaseVersion != snapshot.Metadata.Version ||
		strings.TrimSpace(snapshot.Metadata.PublishedBy) == "" ||
		snapshot.Metadata.PublishedAt.IsZero() ||
		snapshot.Metadata.EffectiveAt.IsZero() {
		return ErrInvalidSnapshot
	}
	if len(snapshot.Prompts) == 0 || len(snapshot.Schemas) == 0 ||
		len(snapshot.Routes) == 0 || len(snapshot.Pricing) == 0 {
		return ErrInvalidSnapshot
	}

	for name, prompt := range snapshot.Prompts {
		if strings.TrimSpace(name) == "" ||
			strings.TrimSpace(prompt.Name) == "" ||
			strings.TrimSpace(prompt.Version) == "" ||
			strings.TrimSpace(prompt.Template) == "" {
			return ErrInvalidSnapshot
		}
	}
	for kind, schema := range snapshot.Schemas {
		if strings.TrimSpace(kind) == "" ||
			strings.TrimSpace(schema.Kind) == "" ||
			strings.TrimSpace(schema.Version) == "" {
			return ErrInvalidSnapshot
		}
	}
	for task, route := range snapshot.Routes {
		if strings.TrimSpace(task) == "" ||
			strings.TrimSpace(route.Task) == "" ||
			strings.TrimSpace(route.Version) == "" ||
			strings.TrimSpace(route.Provider) == "" ||
			strings.TrimSpace(route.Model) == "" ||
			route.Temperature < 0 ||
			route.Temperature > 2 ||
			route.MaxTokens < 0 ||
			route.Timeout <= 0 {
			return ErrInvalidSnapshot
		}
	}
	for key, pricing := range snapshot.Pricing {
		if strings.TrimSpace(key) == "" ||
			strings.TrimSpace(pricing.Provider) == "" ||
			strings.TrimSpace(pricing.Model) == "" ||
			strings.TrimSpace(pricing.Version) == "" ||
			strings.TrimSpace(pricing.Currency) == "" ||
			pricing.InputPer1K < 0 ||
			pricing.OutputPer1K < 0 {
			return ErrInvalidSnapshot
		}
	}
	return nil
}

func cloneSnapshot(snapshot ConfigSnapshot) ConfigSnapshot {
	// 配置快照包含多个 map，仅复制外层结构是不够的。
	// 这里复制所有 map，保证发布版本和 Run 快照不会被外部修改污染。
	clone := snapshot
	clone.Prompts = cloneMap(snapshot.Prompts)
	clone.Schemas = cloneMap(snapshot.Schemas)
	clone.Routes = cloneMap(snapshot.Routes)
	clone.Pricing = cloneMap(snapshot.Pricing)
	return clone
}

func cloneRunSnapshot(snapshot RunSnapshot) RunSnapshot {
	// RunSnapshot 还包含一层 ConfigSnapshot，因此需要继续复制内部配置 map。
	snapshot.Snapshot = cloneSnapshot(snapshot.Snapshot)
	return snapshot
}

func cloneMap[K comparable, V any](source map[K]V) map[K]V {
	if source == nil {
		return nil
	}
	target := make(map[K]V, len(source))
	for key, value := range source {
		target[key] = value
	}
	return target
}
