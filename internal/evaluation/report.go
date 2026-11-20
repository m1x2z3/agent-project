package evaluation

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// RenderMarkdown 将评测结果渲染为不包含敏感原文的 Markdown 报告。
func RenderMarkdown(report Report) string {
	var builder strings.Builder
	builder.WriteString("# 第二周第 6 天评测与发布复盘\n\n")
	builder.WriteString("## 执行摘要\n\n")
	fmt.Fprintf(&builder, "- 生成时间：`%s`\n", report.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(&builder, "- 评测集：`%s`\n", report.Dataset)
	fmt.Fprintf(&builder, "- 每条样例重复次数：`%d`\n", report.Repetitions)
	builder.WriteString("- 评测方式：确定性 Fake Provider 离线评测，不访问真实模型服务。\n")
	builder.WriteString("- `deepseek` 和 `kimi` 仅作为离线供应商身份，不代表真实供应商质量结论。\n\n")

	builder.WriteString("## 供应商结果\n\n")
	builder.WriteString("| provider | release | samples | successes | structured | parse failures | repairs | stability | P50 | P95 | total tokens | cost |\n")
	builder.WriteString("| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |\n")
	for _, summary := range report.Summaries {
		fmt.Fprintf(&builder, "| %s | %s | %d | %d | %d | %d | %d | %.2f | %s | %s | %d | %.6f |\n",
			summary.Provider, summary.ReleaseVersion, summary.Samples, summary.Successes,
			summary.StructuredSuccesses, summary.ParseFailures, summary.RepairCount,
			summary.StabilityRate, formatDuration(summary.P50Latency),
			formatDuration(summary.P95Latency), summary.TotalTokens, summary.TotalCost)
	}
	builder.WriteString("\n")

	builder.WriteString("## 发布演示\n\n")
	fmt.Fprintf(&builder, "- 主运行版本：`%s`，成功：`%t`\n",
		report.Demo.MainReleaseVersion, report.Demo.MainSuccess)
	fmt.Fprintf(&builder, "- 影子运行版本：`%s`，成功：`%t`，错误分类：`%s`\n",
		report.Demo.ShadowReleaseVersion, report.Demo.ShadowSuccess, report.Demo.ShadowErrorKind)
	fmt.Fprintf(&builder, "- 灰度选择：版本 `%s`，候选命中：`%t`，原因：`%s`\n",
		report.Demo.SelectionVersion, report.Demo.SelectionCandidate, report.Demo.SelectionReason)
	fmt.Fprintf(&builder, "- 发布门禁：`%s`，原因：`%s`\n",
		report.Demo.GateAction, report.Demo.GateReason)
	fmt.Fprintf(&builder, "- 回滚版本：`%s`，PreviousVersion：`%s`，RollbackFrom：`%s`\n",
		report.Demo.RollbackVersion, report.Demo.PreviousVersion, report.Demo.RollbackFrom)
	fmt.Fprintf(&builder, "- 历史重放结果一致：`%t`\n", report.Demo.ReplayMatch)
	builder.WriteString("\n历史版本顺序：")
	if len(report.Demo.HistoryVersions) == 0 {
		builder.WriteString("无\n")
	} else {
		history := append([]string(nil), report.Demo.HistoryVersions...)
		sort.Strings(history)
		fmt.Fprintf(&builder, "`%s`\n", strings.Join(history, "`, `"))
	}

	builder.WriteString("\n## 解释和限制\n\n")
	builder.WriteString("- 候选版本的失败由 Fake Provider 受控注入，用于验证发布阻断和回滚，不代表真实 DeepSeek 或 Kimi 的输出质量。\n")
	builder.WriteString("- 当前 `.env` 使用单供应商兼容配置，本日没有执行真实双供应商调用。\n")
	builder.WriteString("- 报告只保存指标、错误分类、版本关系和输出摘要哈希，不保存完整 Prompt、模型回答、API Key 或供应商原始错误。\n")
	builder.WriteString("- 历史重放使用确定性 Fake Provider；真实模型重新调用仍然属于非确定性比较，不能等同于确定性重放。\n")
	builder.WriteString("- 当前发布、灰度和回滚均为进程内演示，尚未接入数据库、HTTP 管理接口或自动化发布系统。\n")
	return builder.String()
}

func formatDuration(duration time.Duration) string {
	if duration <= 0 {
		return "0ms"
	}
	return fmt.Sprintf("%.2fms", float64(duration.Microseconds())/1000)
}
