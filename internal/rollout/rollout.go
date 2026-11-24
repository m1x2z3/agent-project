package rollout

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
	"strings"
	"time"

	"agent-project/internal/configregistry"
)

var (
	ErrInvalidPolicy     = errors.New("rollout policy is invalid")
	ErrCandidateNotFound = errors.New("rollout candidate version not found")
	ErrSelectionKeyEmpty = errors.New("rollout selection key is empty")
)

// RolloutPolicy 描述一个候选配置版本的本地灰度规则。
//
// 灰度不是一次性让所有请求切换到新版本，而是让一部分请求先使用
// CandidateVersion，其余请求继续使用当前 active 版本。Percentage 控制
// 百分比分流，TenantAllowlist 中的租户则始终命中候选版本。
type RolloutPolicy struct {
	CandidateVersion string
	Percentage       uint8
	TenantAllowlist  map[string]struct{}
}

// SelectionInput 是灰度选择所需的最小请求身份信息。
type SelectionInput struct {
	RequestID string
	TenantID  string
}

// SelectionDecision 是可审计的灰度选择结果。
type SelectionDecision struct {
	ReleaseVersion string
	Candidate      bool
	Bucket         uint8
	Reason         string
}

// Selector 根据注册中心中的 active 版本和候选版本执行本地灰度选择。
//
// 它不是发布或激活配置的组件，而是一个版本分流器：
//
//	active 版本 + 候选版本 + 白名单 + 灰度比例
//	    -> 决定本次请求使用的配置版本
//
// 选择结果通过稳定哈希计算，保证相同租户或请求在重复选择时保持一致。
type Selector struct {
	registry *configregistry.Registry
	policy   RolloutPolicy
}

// NewSelector 创建一个不修改注册中心状态的灰度选择器。
//
// 创建时会校验候选版本已经发布，但不会激活候选版本，也不会立即为
// 某个请求做选择。真正的版本选择由 Select 完成。
func NewSelector(registry *configregistry.Registry, policy RolloutPolicy) (*Selector, error) {
	// 候选版本必须先存在于 Registry，避免选择器创建成功后才发现配置不存在。
	if registry == nil || strings.TrimSpace(policy.CandidateVersion) == "" || policy.Percentage > 100 {
		return nil, ErrInvalidPolicy
	}
	if _, err := registry.Get(policy.CandidateVersion); err != nil {
		return nil, ErrCandidateNotFound
	}
	return &Selector{
		registry: registry,
		policy:   clonePolicy(policy),
	}, nil
}

// Policy 返回选择器使用的策略副本，供影子运行等上层编排读取候选版本。
func (s *Selector) Policy() RolloutPolicy {
	if s == nil {
		return RolloutPolicy{}
	}
	return clonePolicy(s.policy)
}

// Select 根据租户白名单或稳定哈希选择本次请求使用的配置版本。
//
// 灰度选择顺序如下：
//
//  1. 白名单租户始终使用候选版本；
//  2. 其他请求优先使用 TenantID，没有租户时使用 RequestID；
//  3. 根据稳定哈希得到 0~99 的流量桶；
//  4. Bucket < Percentage 时使用候选版本，否则使用当前 active 版本。
//
// 例如 Percentage=10 时，桶值 0~9 的请求进入候选版本，其他请求继续
// 使用 active 版本。由于分桶键稳定，同一租户或请求的结果可以重复复现。
func (s *Selector) Select(input SelectionInput) (SelectionDecision, error) {
	if s == nil || s.registry == nil {
		return SelectionDecision{}, ErrInvalidPolicy
	}
	active, err := s.registry.Active()
	if err != nil {
		return SelectionDecision{}, err
	}
	if _, err := s.registry.Get(s.policy.CandidateVersion); err != nil {
		return SelectionDecision{}, ErrCandidateNotFound
	}

	// 优先使用租户作为分桶键；没有租户上下文时才退回 RequestID。
	// 这样同一租户的请求可以保持稳定，而匿名请求仍然能够灰度。
	tenantID := strings.TrimSpace(input.TenantID)
	requestID := strings.TrimSpace(input.RequestID)
	key := tenantID
	if key == "" {
		key = requestID
	}
	if key == "" {
		return SelectionDecision{}, ErrSelectionKeyEmpty
	}
	bucket := stableBucket(key)
	// 白名单优先于百分比：白名单租户始终命中候选版本，
	// 其余请求再按照稳定桶和灰度比例决定版本。
	if _, ok := s.policy.TenantAllowlist[tenantID]; tenantID != "" && ok {
		return SelectionDecision{
			ReleaseVersion: s.policy.CandidateVersion,
			Candidate:      true,
			Bucket:         bucket,
			Reason:         "tenant_allowlist",
		}, nil
	}

	if bucket < s.policy.Percentage {
		return SelectionDecision{
			ReleaseVersion: s.policy.CandidateVersion,
			Candidate:      true,
			Bucket:         bucket,
			Reason:         "percentage_match",
		}, nil
	}
	return SelectionDecision{
		ReleaseVersion: active.ReleaseVersion,
		Candidate:      false,
		Bucket:         bucket,
		Reason:         "active_version",
	}, nil
}

func stableBucket(key string) uint8 {
	// SHA-256 提供稳定且均匀的输入分布；取前 8 字节转成整数后
	// 对 100 取模，得到固定的 0~99 流量桶。
	sum := sha256.Sum256([]byte(key))
	return uint8(binary.BigEndian.Uint64(sum[:8]) % 100)
}

func clonePolicy(policy RolloutPolicy) RolloutPolicy {
	// TenantAllowlist 是 map，必须复制后再对外返回，避免调用方修改选择器内部策略。
	clone := policy
	if policy.TenantAllowlist != nil {
		clone.TenantAllowlist = make(map[string]struct{}, len(policy.TenantAllowlist))
		for tenant := range policy.TenantAllowlist {
			clone.TenantAllowlist[tenant] = struct{}{}
		}
	}
	return clone
}

// GateMetrics 是一组候选版本的运行质量指标。
type GateMetrics struct {
	Samples             int
	Successes           int
	StructuredSuccesses int
	Latencies           []time.Duration
	TotalCost           float64
}

// GatePolicy 定义候选版本通过发布门禁需要满足的最低质量。
type GatePolicy struct {
	MinSamples               int
	MinSuccessRate           float64
	MinStructuredSuccessRate float64
	MaxP95Latency            time.Duration
	MaxAverageCost           float64
}

// GateDecision 是确定性的发布建议，不会自动修改 Registry。
type GateDecision struct {
	Action string
	Reason string
}

const (
	GateAllow             = "allow"
	GateHold              = "hold"
	GateBlock             = "block"
	GateRollbackCandidate = "rollback_candidate"
)

// EvaluateGate 根据样本量、成功率、结构化成功率、P95 延迟和平均成本评估发布。
func EvaluateGate(metrics GateMetrics, policy GatePolicy) GateDecision {
	// 先检查样本是否足够，再依次判断成功率、结构化成功率、P95 延迟和平均成本。
	// 该函数只给出确定性建议，不会自动激活或回滚任何版本。
	if metrics.Samples < policy.MinSamples {
		return GateDecision{Action: GateHold, Reason: "insufficient_samples"}
	}
	if metrics.Samples <= 0 || metrics.Successes < 0 ||
		metrics.StructuredSuccesses < 0 ||
		metrics.Successes > metrics.Samples ||
		metrics.StructuredSuccesses > metrics.Samples {
		return GateDecision{Action: GateBlock, Reason: "invalid_metrics"}
	}

	successRate := float64(metrics.Successes) / float64(metrics.Samples)
	structuredRate := float64(metrics.StructuredSuccesses) / float64(metrics.Samples)
	if len(metrics.Latencies) == 0 {
		return GateDecision{Action: GateBlock, Reason: "latency_samples_missing"}
	}
	p95 := percentile95(metrics.Latencies)
	averageCost := metrics.TotalCost / float64(metrics.Samples)

	if successRate == 0 || structuredRate == 0 {
		return GateDecision{Action: GateRollbackCandidate, Reason: "zero_success_or_structured_rate"}
	}
	switch {
	case successRate < policy.MinSuccessRate:
		return GateDecision{Action: GateBlock, Reason: "success_rate_below_threshold"}
	case structuredRate < policy.MinStructuredSuccessRate:
		return GateDecision{Action: GateBlock, Reason: "structured_success_rate_below_threshold"}
	case p95 > policy.MaxP95Latency:
		return GateDecision{Action: GateBlock, Reason: "p95_latency_above_threshold"}
	case averageCost > policy.MaxAverageCost:
		return GateDecision{Action: GateBlock, Reason: "average_cost_above_threshold"}
	default:
		return GateDecision{Action: GateAllow, Reason: "all_gate_metrics_passed"}
	}
}

func percentile95(latencies []time.Duration) time.Duration {
	if len(latencies) == 0 {
		return 0
	}
	// 排序会修改切片，因此先复制一份，避免污染调用方保存的原始延迟数据。
	values := append([]time.Duration(nil), latencies...)
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	index := (len(values)*95 + 99) / 100
	if index < 1 {
		index = 1
	}
	if index > len(values) {
		index = len(values)
	}
	return values[index-1]
}
