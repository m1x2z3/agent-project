package recording

import (
	"context"
	"testing"
	"time"
)

// TestSystemClock 验证系统时钟实现和当前时间范围。
func TestSystemClock(t *testing.T) {
	var clock Clock = SystemClock{}
	before := time.Now()
	got := clock.Now()
	after := time.Now()
	if got.IsZero() {
		t.Fatal("SystemClock.Now() returned zero time")
	}
	if got.Before(before) || got.After(after) {
		t.Fatalf("SystemClock.Now() = %v, want between %v and %v", got, before, after)
	}
	// 直接比较原始系统时间，确保生产实现没有提前 UTC 化或丢弃单调时钟信息。
	if got.Sub(before) < 0 || after.Sub(got) < 0 {
		t.Fatalf("monotonic clock order is invalid: before=%v got=%v after=%v", before, got, after)
	}
}

// TestWithClock 验证请求级注入、nil Context 和空时钟默认值。
func TestWithClock(t *testing.T) {
	ctx := WithClock(nil, nil)
	if ctx == nil {
		t.Fatal("WithClock() returned nil Context")
	}
	if _, ok := ctx.Value(clockContextKey{}).(SystemClock); !ok {
		t.Fatalf("WithClock(nil, nil) clock = %T, want SystemClock", ctx.Value(clockContextKey{}))
	}

	clock := &scriptedTestClock{}
	ctx = WithClock(ctx, clock)
	if got, ok := ctx.Value(clockContextKey{}).(Clock); !ok || got != clock {
		t.Fatalf("WithClock() clock = %T, want injected clock", ctx.Value(clockContextKey{}))
	}
}

// scriptedTestClock 是只用于契约测试的空 Clock 实现。
type scriptedTestClock struct{}

// Now 返回固定零时间；本测试只验证依赖注入，不读取该时间。
func (*scriptedTestClock) Now() time.Time {
	return time.Time{}
}

// TestClockFromContext 验证注入读取以及 nil、缺省 Context 的系统时钟回退。
func TestClockFromContext(t *testing.T) {
	if _, ok := ClockFromContext(nil).(SystemClock); !ok {
		t.Fatalf("ClockFromContext(nil) = %T, want SystemClock", ClockFromContext(nil))
	}
	if _, ok := ClockFromContext(context.Background()).(SystemClock); !ok {
		t.Fatal("ClockFromContext(empty) did not return SystemClock")
	}
	clock := &scriptedTestClock{}
	if got := ClockFromContext(WithClock(context.Background(), clock)); got != clock {
		t.Fatalf("ClockFromContext(injected) = %T, want injected clock", got)
	}
}
