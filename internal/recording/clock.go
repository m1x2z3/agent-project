// Package recording 提供可验证执行所需的独立依赖录制契约。
// 本包不连接 RunStore、Observer 或外部持久化系统。
package recording

import (
	"context"
	"time"
)

type clockContextKey struct{}

// WithClock 将当前请求独立的 Clock 放入 Context。
// 空调钟使用 SystemClock；每次 Run 应注入自己的序列化测试时钟，避免并发消耗互相影响。
func WithClock(ctx context.Context, clock Clock) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if clock == nil {
		clock = SystemClock{}
	}
	return context.WithValue(ctx, clockContextKey{}, clock)
}

// ClockFromContext 读取当前请求注入的 Clock。
// Context 为空或未配置时返回 SystemClock，保持既有生产调用的时间语义。
func ClockFromContext(ctx context.Context) Clock {
	if ctx != nil {
		if clock, ok := ctx.Value(clockContextKey{}).(Clock); ok && clock != nil {
			return clock
		}
	}
	return SystemClock{}
}

// Clock 为执行链路提供统一的时间读取边界。
//
// 直接调用 time.Now 会让测试只能依赖真实等待。例如验证“一秒后超预算”时，
// 没有 Clock 就只能 Sleep，既缓慢又容易受调度影响；注入测试 Clock 后，可以让
// 第一次 Now 返回 10:00:00、第二次返回 10:00:02，无需等待即可稳定验证超时。
// 同一个 Clock 还可供 Run 预算、RunStore 记录和工具令牌校验使用，保证各边界
// 按相同时间顺序执行。后续依赖重放也可以消费录制时间，而不是重新读取真实时钟。
// 生产环境使用系统时钟，确定性测试可以注入按录制顺序返回时间的实现。
type Clock interface {
	Now() time.Time // 返回当前时间；内部耗时计算应保留 time.Time 的单调时钟信息。
}

// SystemClock 使用进程的真实系统时间。
type SystemClock struct{}

// Now 返回包含可用单调时钟信息的当前系统时间。
func (SystemClock) Now() time.Time {
	return time.Now()
}
