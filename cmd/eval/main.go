package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"agent-project/internal/evaluation"
)

func main() {
	datasetPath := flag.String("dataset", "evals/week2-day6.jsonl", "评测集路径")
	outputPath := flag.String("output", "docs/evidence/2026-11-20/week2-day6.md", "报告输出路径")
	repetitions := flag.Int("repetitions", 3, "每条样例重复执行次数")
	flag.Parse()

	if err := run(*datasetPath, *outputPath, *repetitions); err != nil {
		fmt.Fprintf(os.Stderr, "离线评测失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("离线评测完成，报告已写入 %s\n", *outputPath)
}

func run(datasetPath, outputPath string, repetitions int) error {
	file, err := os.Open(datasetPath)
	if err != nil {
		return fmt.Errorf("打开评测集: %w", err)
	}
	defer file.Close()

	cases, err := evaluation.LoadCases(file)
	if err != nil {
		return err
	}
	runner := evaluation.Runner{
		Repetitions: repetitions,
		Factory:     evaluation.NewOfflineRuntimeFactory(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	deepseekSummary, _, err := runner.Evaluate(ctx, "deepseek", "v1", cases)
	if err != nil {
		return fmt.Errorf("评测 deepseek: %w", err)
	}
	kimiSummary, _, err := runner.Evaluate(ctx, "kimi", "v1", cases)
	if err != nil {
		return fmt.Errorf("评测 kimi: %w", err)
	}
	candidateSummary, _, err := runner.Evaluate(ctx, "kimi", "v2", cases)
	if err != nil {
		return fmt.Errorf("评测候选版本: %w", err)
	}

	demoCase := findCandidateCase(cases)
	if demoCase == nil {
		return fmt.Errorf("评测集缺少 candidate_failure 样例")
	}
	demo, err := evaluation.RunReleaseDemo(ctx, *demoCase, candidateSummary)
	if err != nil {
		return fmt.Errorf("执行发布演示: %w", err)
	}

	report := evaluation.Report{
		GeneratedAt: time.Now().UTC(),
		Dataset:     datasetPath,
		Repetitions: repetitions,
		Summaries:   []evaluation.Summary{deepseekSummary, kimiSummary, candidateSummary},
		Demo:        demo,
	}
	content := evaluation.RenderMarkdown(report)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return fmt.Errorf("创建报告目录: %w", err)
	}
	if err := os.WriteFile(outputPath, []byte(content), 0600); err != nil {
		return fmt.Errorf("写入评测报告: %w", err)
	}
	return nil
}

func findCandidateCase(cases []evaluation.Case) *evaluation.Case {
	for index := range cases {
		if cases[index].CandidateFailure {
			return &cases[index]
		}
	}
	return nil
}
