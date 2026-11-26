package agent

import "testing"

// TestEstimateTokenCount 验证空输入、向上取整、Unicode 和确定性估算规则。
func TestEstimateTokenCount(t *testing.T) {
	tests := []struct {
		name string // 描述当前估算场景。
		text string // 保存待估算的文本。
		want int    // 保存期望的本地估算 Token 数。
	}{
		{name: "empty", text: "", want: 0},
		{name: "one ASCII rune", text: "a", want: 1},
		{name: "four ASCII runes", text: "abcd", want: 1},
		{name: "five ASCII runes rounds up", text: "abcde", want: 2},
		{name: "four Chinese runes", text: "预算停止", want: 1},
		{name: "five Chinese runes", text: "预算停止了", want: 2},
		{name: "mixed Unicode", text: "ab预", want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := estimateTokenCount(tt.text); got != tt.want {
				t.Fatalf("estimateTokenCount(%q) = %d, want %d", tt.text, got, tt.want)
			}
			// 同一输入再次计算必须一致，不能依赖时间、随机数或供应商。
			if got := estimateTokenCount(tt.text); got != tt.want {
				t.Fatalf("second estimateTokenCount(%q) = %d, want %d", tt.text, got, tt.want)
			}
		})
	}
}
