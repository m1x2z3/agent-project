// Package memory 提供 runstore.Store 的进程内内存实现。
package memory

import (
	"context"
	"strings"
	"sync"
	"time"

	"agent-project/internal/clone"
	"agent-project/internal/runstore"
)

// MemoryStore 是 Run 记录和事件的线程安全内存存储。
//
// runs 保存每个 Run 的当前记录，events 保存每个 Run 的事件列表，
// nextSequence 保存各 Run 下一条事件应使用的序号。后续方法会通过
// mutex 保护这些状态，并在返回数据前执行深拷贝。
type MemoryStore struct {
	mu           sync.RWMutex
	runs         map[string]runstore.RunRecord
	events       map[string][]runstore.RunEvent
	nextSequence map[string]uint64
}

var _ runstore.Store = (*MemoryStore)(nil)

// NewMemoryStore 创建一个可以直接使用的空内存存储。
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		runs:         make(map[string]runstore.RunRecord),
		events:       make(map[string][]runstore.RunEvent),
		nextSequence: make(map[string]uint64),
	}
}

// CreateRun 创建一条处于 running 状态的 Run 记录。
//
// 创建时只保存配置版本引用，不保存完整配置内容；同时初始化该 Run
// 的事件列表和下一条事件序号。调用方传入的版本 Map 会被复制，避免
// 后续修改原对象污染存储内部状态。
func (s *MemoryStore) CreateRun(ctx context.Context, record runstore.RunRecord) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if s == nil ||
		strings.TrimSpace(record.RunID) == "" ||
		strings.TrimSpace(record.RequestID) == "" ||
		strings.TrimSpace(record.TenantID) == "" ||
		record.Status != runstore.RunRunning ||
		strings.TrimSpace(record.Config.ReleaseVersion) == "" ||
		record.StartedAt.IsZero() ||
		!record.FinishedAt.IsZero() {
		return runstore.ErrInvalidRun
	}

	normalized := record
	normalized.Config.PromptVersions = clone.Map(record.Config.PromptVersions)
	normalized.Config.SchemaVersions = clone.Map(record.Config.SchemaVersions)
	normalized.Config.RouteVersions = clone.Map(record.Config.RouteVersions)
	normalized.Config.PricingVersions = clone.Map(record.Config.PricingVersions)
	normalized.StartedAt = normalized.StartedAt.UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.runs[normalized.RunID]; exists {
		return runstore.ErrRunAlreadyExists
	}
	s.runs[normalized.RunID] = normalized
	s.events[normalized.RunID] = nil
	s.nextSequence[normalized.RunID] = 1
	return nil
}

// AppendEvent 向已存在且仍在运行的 Run 追加一个事件。
//
// 事件序号由存储层统一分配，调用方传入的 Sequence 会被覆盖。事件中的
// Run、Request 和 Tenant 标识必须与已保存 Run 一致，避免把不同执行上下文
// 的数据写入同一条运行记录。
func (s *MemoryStore) AppendEvent(ctx context.Context, event runstore.RunEvent) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if s == nil ||
		strings.TrimSpace(event.RunID) == "" ||
		strings.TrimSpace(event.RequestID) == "" ||
		strings.TrimSpace(event.TenantID) == "" ||
		event.OccurredAt.IsZero() ||
		event.DurationMS < 0 {
		return runstore.ErrInvalidEvent
	}

	switch event.Type {
	case runstore.EventRunStarted,
		runstore.EventModelCall,
		runstore.EventToolCall,
		runstore.EventToolResult,
		runstore.EventRunSucceeded,
		runstore.EventRunFailed:
	default:
		return runstore.ErrInvalidEventType
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	record, exists := s.runs[event.RunID]
	if !exists {
		return runstore.ErrRunNotFound
	}
	if record.RequestID != event.RequestID || record.TenantID != event.TenantID {
		return runstore.ErrRunIdentityMismatch
	}
	if record.Status != runstore.RunRunning {
		return runstore.ErrRunAlreadyFinished
	}

	event.Sequence = s.nextSequence[event.RunID]
	s.events[event.RunID] = append(s.events[event.RunID], event)
	s.nextSequence[event.RunID]++
	return nil
}

// FinishRun 结束一条仍处于 running 状态的 Run。
//
// 只有 succeeded、failed 和 canceled 可以作为结束状态。方法会统一写入
// 当前 UTC 时间，并保存最终脱敏观测；已经结束的 Run 不能重复完成。
func (s *MemoryStore) FinishRun(
	ctx context.Context,
	runID string,
	status runstore.RunStatus,
	observation runstore.RunObservation,
) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if s == nil || strings.TrimSpace(runID) == "" {
		return runstore.ErrInvalidRun
	}
	switch status {
	case runstore.RunSucceeded, runstore.RunFailed, runstore.RunCanceled:
	default:
		return runstore.ErrInvalidStateTransition
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	record, exists := s.runs[runID]
	if !exists {
		return runstore.ErrRunNotFound
	}
	if record.Status != runstore.RunRunning {
		return runstore.ErrRunAlreadyFinished
	}
	record.Status = status
	record.FinishedAt = time.Now().UTC()
	record.Observation = observation
	s.runs[runID] = record
	return nil
}

// GetRun 查询指定 Run，并返回不会暴露内部 Map 的独立副本。
func (s *MemoryStore) GetRun(ctx context.Context, runID string) (runstore.RunRecord, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return runstore.RunRecord{}, err
		}
	}
	if s == nil || strings.TrimSpace(runID) == "" {
		return runstore.RunRecord{}, runstore.ErrInvalidRun
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	record, exists := s.runs[runID]
	if !exists {
		return runstore.RunRecord{}, runstore.ErrRunNotFound
	}
	record.Config.PromptVersions = clone.Map(record.Config.PromptVersions)
	record.Config.SchemaVersions = clone.Map(record.Config.SchemaVersions)
	record.Config.RouteVersions = clone.Map(record.Config.RouteVersions)
	record.Config.PricingVersions = clone.Map(record.Config.PricingVersions)
	return record, nil
}

// ListEvents 按追加顺序查询指定 Run 的事件，并返回独立切片。
func (s *MemoryStore) ListEvents(ctx context.Context, runID string) ([]runstore.RunEvent, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if s == nil || strings.TrimSpace(runID) == "" {
		return nil, runstore.ErrInvalidRun
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	if _, exists := s.runs[runID]; !exists {
		return nil, runstore.ErrRunNotFound
	}
	events := s.events[runID]
	if len(events) == 0 {
		return []runstore.RunEvent{}, nil
	}
	cloned := make([]runstore.RunEvent, len(events))
	copy(cloned, events)
	return cloned, nil
}
