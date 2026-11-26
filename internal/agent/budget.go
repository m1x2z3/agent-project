package agent

import (
	"context"
	"errors"
	"time"
)

// BudgetScope 标识一项预算限制所属的可信作用域。
// 它用于说明有效预算来自用户、租户还是单次 Run，不接受模型输出作为来源。
type BudgetScope string

const BudgetScopeUser BudgetScope = "user"       // 标识用户级预算来源。
const BudgetScopeTenant BudgetScope = "tenant"   // 标识租户级预算来源。
const BudgetScopeRun BudgetScope = "run"         // 标识单次 Run 级预算来源。
const BudgetScopeDefault BudgetScope = "default" // 标识本地安全默认预算来源。

var ErrInvalidBudget = errors.New("agent budget is invalid")   // 表示预算配置包含负数或其他非法上限。
var ErrBudgetExceeded = errors.New("agent budget is exceeded") // 表示一次 Run 已达到确定性预算终止条件。

// BudgetViolation 描述一次可安全记录和返回的预算超限事实。
// 该结构只包含稳定分类和数值，不携带 Prompt、模型回答或工具参数。
type BudgetViolation struct {
	Scope  BudgetScope // 标识最终生效限制来自用户、租户、Run 或默认值。
	Reason string      // 保存 max_steps_exceeded 等稳定预算原因。
	Limit  int64       // 保存生效限额；时间预算统一使用毫秒。
	Used   int64       // 保存触发终止时的消耗；时间预算统一使用毫秒。
}

// Error 返回不包含敏感业务内容的稳定预算错误文本。
// 数值消耗保留在结构化字段中，调用方可以通过 errors.As 读取完整违规信息。
func (v BudgetViolation) Error() string {
	if v.Reason == "" {
		return ErrBudgetExceeded.Error()
	}
	if v.Scope == "" {
		return ErrBudgetExceeded.Error() + ": " + v.Reason
	}
	return ErrBudgetExceeded.Error() + ": " + v.Reason + " scope=" + string(v.Scope)
}

// Unwrap 将具体预算违规关联到统一的 ErrBudgetExceeded。
// 调用方既可以 errors.Is 判断预算终止，也可以 errors.As 读取结构化违规字段。
func (v BudgetViolation) Unwrap() error {
	return ErrBudgetExceeded
}

// BudgetController 保存一次 Run 的预算上限和独立执行消耗。
// 每个 Run 必须持有独立实例，不能把消耗字段共享给并发执行的其他 Run。
type BudgetController struct {
	effective       EffectiveBudget // 保存本次 Run 合并后的有效预算和来源。
	started         time.Time       // 保存计算 Run 总时间预算的开始时间。
	steps           int64           // 记录已经进入执行边界的模型、工具和修复步数。
	toolCalls       int64           // 记录已经进入执行边界的工具调用次数。
	totalTokens     int64           // 记录跨模型调用累计结算的预算 Token 数。
	estimatedTokens int64           // 记录预算 Token 中来自本地估算的部分，用于标记消耗来源。
}

// newBudgetController 为一次 Run 创建独立的预算执行状态。
// 构造时完成三级预算合并；任一配置非法都不会返回可执行的控制器。
func newBudgetController(set BudgetSet, started time.Time) (*BudgetController, error) {
	effective, err := resolveEffectiveBudget(set)
	if err != nil {
		return nil, err
	}
	return &BudgetController{
		effective: effective,
		started:   started,
	}, nil
}

// checkElapsed 在发起新操作前检查 Run 总时间预算。
// 时钟倒退时按零耗时处理；达到总时限即返回稳定违规，不允许再开始后续操作。
func (c *BudgetController) checkElapsed(now time.Time) error {
	if c == nil {
		return ErrInvalidBudget
	}
	elapsed := now.Sub(c.started)
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed >= c.effective.Limits.MaxDuration {
		return BudgetViolation{
			Scope:  c.effective.DurationSource,
			Reason: "total_time_budget_exceeded",
			Limit:  c.effective.Limits.MaxDuration.Milliseconds(),
			Used:   elapsed.Milliseconds(),
		}
	}
	return nil
}

// RemainingDuration 返回当前 Run 距离总时间上限的剩余时长。
// 时钟倒退按尚未消耗时间处理；已经达到或超过上限时返回零，不再产生负数超时。
func (c *BudgetController) RemainingDuration(now time.Time) time.Duration {
	if c == nil {
		return 0
	}
	elapsed := now.Sub(c.started)
	if elapsed < 0 {
		elapsed = 0
	}
	remaining := c.effective.Limits.MaxDuration - elapsed
	if remaining < 0 {
		return 0
	}
	return remaining
}

// BeginStep 在一次模型、工具或修复操作前检查时间和步数预算。
// 只有全部检查通过后才计步，因此被拒绝的操作不会消耗额外步数。
func (c *BudgetController) BeginStep(now time.Time) error {
	if err := c.checkElapsed(now); err != nil {
		return err
	}
	if c.steps >= int64(c.effective.Limits.MaxSteps) {
		return BudgetViolation{
			Scope:  c.effective.StepsSource,
			Reason: "max_steps_exceeded",
			Limit:  int64(c.effective.Limits.MaxSteps),
			Used:   c.steps,
		}
	}
	c.steps++
	return nil
}

// BeginModelStep 在模型或修复操作前检查 Token、时间和总步数预算。
// 已达到 Token 上限时不允许开始新的模型操作，避免把后续真实用量推到限额之外。
func (c *BudgetController) BeginModelStep(now time.Time) error {
	if c == nil {
		return ErrInvalidBudget
	}
	if c.totalTokens >= int64(c.effective.Limits.MaxTotalTokens) {
		return BudgetViolation{
			Scope:  c.effective.TokensSource,
			Reason: "token_budget_exceeded",
			Limit:  int64(c.effective.Limits.MaxTotalTokens),
			Used:   c.totalTokens,
		}
	}
	return c.BeginStep(now)
}

// ChargeTokens 在一次模型或修复操作结束后结算预算 Token。
// 恰好达到上限允许当前操作完成，超过上限则返回违规，由调用方停止后续阶段。
func (c *BudgetController) ChargeTokens(tokens int) error {
	if c == nil || tokens < 0 {
		return ErrInvalidBudget
	}
	c.totalTokens += int64(tokens)
	if c.totalTokens > int64(c.effective.Limits.MaxTotalTokens) {
		return BudgetViolation{
			Scope:  c.effective.TokensSource,
			Reason: "token_budget_exceeded",
			Limit:  int64(c.effective.Limits.MaxTotalTokens),
			Used:   c.totalTokens,
		}
	}
	return nil
}

// ChargeEstimatedTokens 结算缺少供应商 Usage 时产生的确定性估算消耗。
// 估算值只进入预算账本及其来源统计，不进入 Observer 的真实供应商 Usage。
func (c *BudgetController) ChargeEstimatedTokens(tokens int) error {
	if c == nil || tokens < 0 {
		return ErrInvalidBudget
	}
	// 即使本次结算超过上限，也要同时保留总消耗和其中的估算部分。
	err := c.ChargeTokens(tokens)
	c.estimatedTokens += int64(tokens)
	return err
}

// RemainingTokens 返回当前 Run 尚可结算的 Token 数量。
// 已达到或超过上限时返回零；该值用于约束下一次 MaxTokens，不替代调用后的真实结算。
func (c *BudgetController) RemainingTokens() int {
	if c == nil {
		return 0
	}
	remaining := int64(c.effective.Limits.MaxTotalTokens) - c.totalTokens
	if remaining <= 0 {
		return 0
	}
	return int(remaining)
}

// BeginToolCall 在工具进入执行边界前检查时间、工具次数和总步数预算。
// 工具次数通过后才复用 BeginStep 计入总步数，最后被拒绝的调用不会增加消耗。
func (c *BudgetController) BeginToolCall(now time.Time) error {
	if c == nil {
		return ErrInvalidBudget
	}
	if c.toolCalls >= int64(c.effective.Limits.MaxToolCalls) {
		return BudgetViolation{
			Scope:  c.effective.ToolCallsSource,
			Reason: "max_tool_calls_exceeded",
			Limit:  int64(c.effective.Limits.MaxToolCalls),
			Used:   c.toolCalls,
		}
	}
	if err := c.BeginStep(now); err != nil {
		return err
	}
	c.toolCalls++
	return nil
}

// BudgetSet 保存一次 Run 可用的三级预算限制。
// 各字段分别来自可信用户、租户和 Run 配置，不能直接采用模型或工具返回的值。
type BudgetSet struct {
	User   BudgetLimits // 保存当前用户维度允许使用的预算上限。
	Tenant BudgetLimits // 保存当前租户维度允许使用的预算上限。
	Run    BudgetLimits // 保存当前单次执行额外施加的预算上限。
}

// EffectiveBudget 保存一次 Run 实际执行的预算及各项限制的来源。
// 来源信息用于解释预算终止原因，Limits 才是后续执行控制使用的有效上限。
type EffectiveBudget struct {
	Limits          BudgetLimits // 保存三级预算合并并应用安全默认值后的执行上限。
	StepsSource     BudgetScope  // 标识最大步数限制来自哪个作用域。
	ToolCallsSource BudgetScope  // 标识工具调用次数限制来自哪个作用域。
	TokensSource    BudgetScope  // 标识累计 Token 限制来自哪个作用域。
	DurationSource  BudgetScope  // 标识 Run 总时间限制来自哪个作用域。
}

// BudgetLimits 描述一个预算作用域对单次 Run 施加的执行上限。
//
// 用户、租户和 Run 级配置都使用该结构表达限制；零值字段表示当前作用域
// 未配置对应限制，后续由可信预算合并逻辑选择更严格的有效值。
type BudgetLimits struct {
	MaxSteps       int           // 限制模型调用、工具执行和结构化修复的总步数。
	MaxToolCalls   int           // 限制一次 Run 可以进入执行边界的工具调用次数。
	MaxTotalTokens int           // 限制一次 Run 跨全部模型调用累计消耗的 Token 数。
	MaxDuration    time.Duration // 限制从 Run 开始到最终收口允许消耗的总时间。
}

// validateBudgetLimits 校验单个作用域的预算上限。
// 零值表示该作用域未配置对应限制；负数无法表达有效预算，必须在校验阶段拒绝。
func validateBudgetLimits(limits BudgetLimits) error {
	if limits.MaxSteps < 0 ||
		limits.MaxToolCalls < 0 ||
		limits.MaxTotalTokens < 0 ||
		limits.MaxDuration < 0 {
		return ErrInvalidBudget
	}
	return nil
}

// validateBudgetSet 校验一次 Run 收到的三级预算配置。
// 任一作用域包含非法上限时都终止解析，避免后续合并逻辑忽略错误配置。
func validateBudgetSet(set BudgetSet) error {
	for _, limits := range []BudgetLimits{set.User, set.Tenant, set.Run} {
		if err := validateBudgetLimits(limits); err != nil {
			return err
		}
	}
	return nil
}

// defaultBudgetLimits 返回没有外部预算配置时使用的本地安全默认上限。
// 每次返回新的结构体值，调用方不能通过修改结果影响其他 Run 的默认配置。
func defaultBudgetLimits() BudgetLimits {
	return BudgetLimits{
		MaxSteps:       16,
		MaxToolCalls:   3,
		MaxTotalTokens: 100000,
		MaxDuration:    2 * time.Minute,
	}
}

// resolveEffectiveBudget 将三级预算合并为一次 Run 的实际执行上限。
// 每个维度从零值配置和默认值中选择更严格的正值，同时记录最终限制的来源。
func resolveEffectiveBudget(set BudgetSet) (EffectiveBudget, error) {
	if err := validateBudgetSet(set); err != nil {
		return EffectiveBudget{}, err
	}

	effective := EffectiveBudget{
		Limits:          defaultBudgetLimits(),
		StepsSource:     BudgetScopeDefault,
		ToolCallsSource: BudgetScopeDefault,
		TokensSource:    BudgetScopeDefault,
		DurationSource:  BudgetScopeDefault,
	}
	scopedBudgets := []struct {
		scope  BudgetScope  // 标识当前待合并的预算作用域。
		limits BudgetLimits // 保存该作用域配置的预算上限。
	}{
		{scope: BudgetScopeUser, limits: set.User},
		{scope: BudgetScopeTenant, limits: set.Tenant},
		{scope: BudgetScopeRun, limits: set.Run},
	}

	// 相同上限保留先出现的作用域，确保来源选择稳定；更小的正值会替换默认值
	// 或此前较宽松的限制，未配置的零值不会覆盖任何现有限制。
	for _, scoped := range scopedBudgets {
		if scoped.limits.MaxSteps > 0 && scoped.limits.MaxSteps < effective.Limits.MaxSteps {
			effective.Limits.MaxSteps = scoped.limits.MaxSteps
			effective.StepsSource = scoped.scope
		}
		if scoped.limits.MaxToolCalls > 0 && scoped.limits.MaxToolCalls < effective.Limits.MaxToolCalls {
			effective.Limits.MaxToolCalls = scoped.limits.MaxToolCalls
			effective.ToolCallsSource = scoped.scope
		}
		if scoped.limits.MaxTotalTokens > 0 && scoped.limits.MaxTotalTokens < effective.Limits.MaxTotalTokens {
			effective.Limits.MaxTotalTokens = scoped.limits.MaxTotalTokens
			effective.TokensSource = scoped.scope
		}
		if scoped.limits.MaxDuration > 0 && scoped.limits.MaxDuration < effective.Limits.MaxDuration {
			effective.Limits.MaxDuration = scoped.limits.MaxDuration
			effective.DurationSource = scoped.scope
		}
	}
	return effective, nil
}

// BudgetResolver 定义从可信运行上下文解析三级预算的边界。
// 实现可以读取用户、租户和 Run 配置，但不能把模型或工具输出作为预算来源。
type BudgetResolver interface {
	Resolve(context.Context, RunInput) (BudgetSet, error) // 解析当前 Run 的三级预算，配置不可信或不完整时返回错误。
}

// FixedBudgetResolver 提供本地静态三级预算，供开发配置和集成测试使用。
// 它只返回可信配置，不记录用户或租户跨 Run 的累计消耗，也不冒充生产配额系统。
type FixedBudgetResolver struct {
	Budgets BudgetSet // 保存所有使用此解析器的 Run 共用的静态预算规则。
}

// Resolve 返回当前解析器持有的静态三级预算。
// 每次返回前都检查 Context 和配置合法性；BudgetSet 只含值字段，返回值可以独立修改。
func (r FixedBudgetResolver) Resolve(ctx context.Context, _ RunInput) (BudgetSet, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return BudgetSet{}, err
		}
	}
	if err := validateBudgetSet(r.Budgets); err != nil {
		return BudgetSet{}, err
	}
	return r.Budgets, nil
}
