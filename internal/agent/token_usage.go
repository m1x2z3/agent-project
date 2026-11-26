package agent

import "unicode/utf8"

// estimateTokenCount 在供应商未返回 Usage 时提供确定性的本地 Token 粗估。
//
// 估算按 Unicode 码点数量每 4 个计 1 个 Token，非空文本至少计 1 个；
// 相同输入始终得到相同结果，不依赖供应商、随机数或当前时间。该值只用于
// 预算兜底，不代表真实计费用量，也不能写进 Observer 的真实 Usage 字段。
func estimateTokenCount(text string) int {
	if text == "" {
		return 0
	}
	runes := utf8.RuneCountInString(text)
	return (runes + 3) / 4
}
