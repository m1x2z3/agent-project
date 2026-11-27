package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"agent-project/internal/configregistry"
	"agent-project/internal/rollout"
	"agent-project/internal/runstore"
	"agent-project/internal/runstore/memory"
	"agent-project/internal/structuredoutput"
	toolregistry "agent-project/internal/tool/registry"
	"agent-project/internal/tool/runner"
	"agent-project/internal/tool/tooling"
)

// TestNormalizeContextPolicy 验证零值默认值、自定义值和全部负数校验分支。
func TestNormalizeContextPolicy(t *testing.T) {
	tests := []struct {
		name    string
		policy  ContextPolicy
		want    ContextPolicy
		wantErr bool
	}{
		{
			name:   "zero values use defaults",
			policy: ContextPolicy{},
			want: ContextPolicy{
				MaxHistoryTokens:    12000,
				MaxToolResultTokens: 2560,
				KeepLatestResults:   10,
			},
		},
		{
			name: "custom values are preserved",
			policy: ContextPolicy{
				MaxHistoryTokens:    1800,
				MaxToolResultTokens: 512,
				KeepLatestResults:   4,
			},
			want: ContextPolicy{
				MaxHistoryTokens:    1800,
				MaxToolResultTokens: 512,
				KeepLatestResults:   4,
			},
		},
		{
			name: "only zero fields receive defaults",
			policy: ContextPolicy{
				MaxHistoryTokens:  800,
				KeepLatestResults: 1,
			},
			want: ContextPolicy{
				MaxHistoryTokens:    800,
				MaxToolResultTokens: 2560,
				KeepLatestResults:   1,
			},
		},
		{
			name:    "negative history limit is rejected",
			policy:  ContextPolicy{MaxHistoryTokens: -1},
			wantErr: true,
		},
		{
			name:    "negative result limit is rejected",
			policy:  ContextPolicy{MaxToolResultTokens: -1},
			wantErr: true,
		},
		{
			name:    "negative retained result count is rejected",
			policy:  ContextPolicy{KeepLatestResults: -1},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeContextPolicy(tt.policy)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidContextPolicy) {
					t.Fatalf("normalizeContextPolicy() error = %v, want ErrInvalidContextPolicy", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeContextPolicy() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("normalizeContextPolicy() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestContextPolicyVersion 验证版本格式包含发布版本与实际策略，并保持确定性。
func TestContextPolicyVersion(t *testing.T) {
	policy := ContextPolicy{
		MaxHistoryTokens:    900,
		MaxToolResultTokens: 128,
		KeepLatestResults:   1,
	}
	const want = "context.v1:release-v1:h900:r128:k1"
	if got := policy.contextVersion("release-v1"); got != want {
		t.Fatalf("contextVersion() = %q, want %q", got, want)
	}
	if first, second := policy.contextVersion("release-v1"), policy.contextVersion("release-v1"); first != second {
		t.Fatalf("contextVersion() is not deterministic: %q != %q", first, second)
	}
}

// TestToolLoopRuntimeContextPolicy 验证构造入口保存规范化策略并拒绝非法配置。
func TestToolLoopRuntimeContextPolicy(t *testing.T) {
	t.Run("default policy", func(t *testing.T) {
		runtime, err := NewToolLoopRuntime(newContextPolicyTestConfig(t))
		if err != nil {
			t.Fatalf("NewToolLoopRuntime() error = %v", err)
		}
		want := ContextPolicy{
			MaxHistoryTokens:    12000,
			MaxToolResultTokens: 2560,
			KeepLatestResults:   10,
		}
		if runtime.contextPolicy != want {
			t.Fatalf("runtime contextPolicy = %+v, want %+v", runtime.contextPolicy, want)
		}
		if got, wantVersion := runtime.contextPolicy.contextVersion("v1"), "context.v1:v1:h12000:r2560:k10"; got != wantVersion {
			t.Fatalf("runtime context version = %q, want %q", got, wantVersion)
		}
	})

	t.Run("custom policy", func(t *testing.T) {
		cfg := newContextPolicyTestConfig(t)
		cfg.ContextPolicy = ContextPolicy{
			MaxHistoryTokens:  800,
			KeepLatestResults: 1,
		}
		runtime, err := NewToolLoopRuntime(cfg)
		if err != nil {
			t.Fatalf("NewToolLoopRuntime() error = %v", err)
		}
		want := ContextPolicy{
			MaxHistoryTokens:    800,
			MaxToolResultTokens: 2560,
			KeepLatestResults:   1,
		}
		if runtime.contextPolicy != want {
			t.Fatalf("runtime contextPolicy = %+v, want %+v", runtime.contextPolicy, want)
		}
	})

	t.Run("invalid policy", func(t *testing.T) {
		cfg := newContextPolicyTestConfig(t)
		cfg.ContextPolicy.MaxToolResultTokens = -1
		runtime, err := NewToolLoopRuntime(cfg)
		if !errors.Is(err, ErrInvalidContextPolicy) {
			t.Fatalf("NewToolLoopRuntime() error = %v, want ErrInvalidContextPolicy", err)
		}
		if runtime != nil {
			t.Fatalf("NewToolLoopRuntime() runtime = %+v, want nil", runtime)
		}
	})
}

// TestCompressToolResultDataKeepsShortJSON 验证短结果不压缩、不改写且不共享底层数组。
func TestCompressToolResultDataKeepsShortJSON(t *testing.T) {
	original := json.RawMessage(`{ "text": "短结果", "count": 1 }`)
	originalCopy := append(json.RawMessage(nil), original...)
	got, compressed, err := compressToolResultData(original, 32)
	if err != nil {
		t.Fatalf("compressToolResultData() error = %v", err)
	}
	if compressed {
		t.Fatal("short tool data was compressed")
	}
	if !bytes.Equal(got, original) || !bytes.Equal(original, originalCopy) {
		t.Fatalf("short tool data changed: got %s, original %s", got, original)
	}
	got[0] = '['
	if !bytes.Equal(original, originalCopy) {
		t.Fatal("returned short tool data aliases the original bytes")
	}
}

// TestCompressToolResultDataDeterministic 验证长结果的字段、摘要、预览和确定性。
func TestCompressToolResultDataDeterministic(t *testing.T) {
	data := json.RawMessage(`{"text":"` + strings.Repeat("订单错误🙂", 100) + `","count":1}`)
	originalCopy := append(json.RawMessage(nil), data...)
	const maxTokens = 16
	first, compressed, err := compressToolResultData(data, maxTokens)
	if err != nil {
		t.Fatalf("compressToolResultData() error = %v", err)
	}
	if !compressed {
		t.Fatal("long tool data was not compressed")
	}
	second, secondCompressed, err := compressToolResultData(data, maxTokens)
	if err != nil {
		t.Fatalf("second compressToolResultData() error = %v", err)
	}
	if !secondCompressed || !bytes.Equal(first, second) {
		t.Fatalf("compression is not deterministic: first %s, second %s", first, second)
	}
	if !json.Valid(first) {
		t.Fatalf("compressed tool data is invalid JSON: %s", first)
	}
	if len(first) >= len(data) {
		t.Fatalf("compressed size = %d, original size = %d", len(first), len(data))
	}
	if !bytes.Equal(data, originalCopy) {
		t.Fatal("compression modified the original tool data")
	}

	var decoded struct {
		Compressed     bool   `json:"compressed"`
		OriginalTokens int    `json:"original_tokens"`
		Digest         string `json:"digest"`
		Preview        string `json:"preview"`
	}
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	sum := sha256.Sum256(data)
	wantDigest := "sha256:" + hex.EncodeToString(sum[:])
	if !decoded.Compressed ||
		decoded.OriginalTokens != estimateTokenCount(string(data)) ||
		decoded.Digest != wantDigest {
		t.Fatalf("compressed metadata = %+v", decoded)
	}
	if decoded.Preview == "" || !utf8.ValidString(decoded.Preview) {
		t.Fatalf("preview is empty or invalid UTF-8: %q", decoded.Preview)
	}
	if got, want := utf8.RuneCountInString(decoded.Preview), maxTokens*4; got != want {
		t.Fatalf("preview runes = %d, want %d", got, want)
	}
	if !strings.HasPrefix(string(data), decoded.Preview) {
		t.Fatalf("preview is not a prefix of compact original data: %q", decoded.Preview)
	}
}

// TestCompressToolResultDataRejectsInvalidInput 验证非法 JSON 和非法目标不会被压缩掩盖。
func TestCompressToolResultDataRejectsInvalidInput(t *testing.T) {
	longInvalidJSON := json.RawMessage(`{"text":"` + strings.Repeat("x", 200))
	got, compressed, err := compressToolResultData(longInvalidJSON, 8)
	if !errors.Is(err, ErrToolHistoryEncodingFailed) {
		t.Fatalf("invalid JSON error = %v, want ErrToolHistoryEncodingFailed", err)
	}
	if got != nil || compressed {
		t.Fatalf("invalid JSON result = %s, compressed = %v", got, compressed)
	}

	for _, maxTokens := range []int{0, -1} {
		got, compressed, err = compressToolResultData(json.RawMessage(`{"ok":true}`), maxTokens)
		if !errors.Is(err, ErrInvalidContextPolicy) {
			t.Fatalf("maxTokens = %d error = %v, want ErrInvalidContextPolicy", maxTokens, err)
		}
		if got != nil || compressed {
			t.Fatalf("maxTokens = %d result = %s, compressed = %v", maxTokens, got, compressed)
		}
	}
}

// TestEncodeGovernedToolHistoryShortHistory 验证版本化 envelope、确定性和原始历史不可变。
func TestEncodeGovernedToolHistoryShortHistory(t *testing.T) {
	history := newContextGovernanceHistory()[:2]
	original := cloneContextGovernanceHistory(t, history)
	policy := ContextPolicy{
		MaxHistoryTokens:    100000,
		MaxToolResultTokens: 10000,
		KeepLatestResults:   1,
	}
	first, report, err := encodeGovernedToolHistory(history, policy, "release-v1")
	if err != nil {
		t.Fatalf("encodeGovernedToolHistory() error = %v", err)
	}
	second, secondReport, err := encodeGovernedToolHistory(history, policy, "release-v1")
	if err != nil {
		t.Fatalf("second encodeGovernedToolHistory() error = %v", err)
	}
	if first != second || report != secondReport {
		t.Fatalf("governed history is not deterministic: first %s, second %s", first, second)
	}
	if report.trimmed {
		t.Fatal("short history was unexpectedly trimmed")
	}
	envelope := decodeContextGovernanceEnvelope(t, first)
	if envelope.ContextVersion != "context.v1:release-v1:h100000:r10000:k1" ||
		envelope.OmittedEntries != 0 ||
		len(envelope.Entries) != 2 {
		t.Fatalf("history envelope = %+v", envelope)
	}
	entry := envelope.Entries[0]
	if entry.ToolName != "old-0" ||
		entry.ToolVersion != "v1" ||
		entry.Status != string(tooling.ResultSuccess) ||
		entry.ArgumentsDigest != "sha256:old-0" ||
		len(entry.Data) == 0 ||
		len(entry.Source) != 1 ||
		entry.Source[0].Source != "source-old-0" {
		t.Fatalf("preserved history entry = %+v", entry)
	}
	if !reflect.DeepEqual(history, original) {
		t.Fatalf("original history changed: got %+v, want %+v", history, original)
	}
}

// TestEncodeGovernedToolHistoryCompression 验证压缩后仍保留工具元数据和来源。
func TestEncodeGovernedToolHistoryCompression(t *testing.T) {
	history := []toolHistoryEntry{{
		Record: ToolCallRecord{
			ToolName:        "search_project_docs",
			ToolVersion:     "v2",
			Status:          tooling.ResultTimeout,
			ErrorCode:       "timeout",
			ArgumentsDigest: "sha256:arguments",
		},
		Data: json.RawMessage(`{"text":"` + strings.Repeat("订单错误🙂", 100) + `"}`),
		Source: []tooling.SourceRef{{
			Source:   "project_docs",
			Location: "project-a/doc-1",
		}},
	}}
	original := cloneContextGovernanceHistory(t, history)
	encoded, report, err := encodeGovernedToolHistory(history, ContextPolicy{
		MaxHistoryTokens:    100000,
		MaxToolResultTokens: 8,
		KeepLatestResults:   1,
	}, "release-v1")
	if err != nil {
		t.Fatalf("encodeGovernedToolHistory() error = %v", err)
	}
	if !report.trimmed {
		t.Fatal("compression was not included in the governance report")
	}
	envelope := decodeContextGovernanceEnvelope(t, encoded)
	if len(envelope.Entries) != 1 || envelope.OmittedEntries != 0 {
		t.Fatalf("history envelope = %+v", envelope)
	}
	entry := envelope.Entries[0]
	var data struct {
		Compressed bool `json:"compressed"`
	}
	if err := json.Unmarshal(entry.Data, &data); err != nil {
		t.Fatalf("Unmarshal(data) error = %v", err)
	}
	if !data.Compressed ||
		entry.ToolName != "search_project_docs" ||
		entry.ToolVersion != "v2" ||
		entry.Status != string(tooling.ResultTimeout) ||
		entry.ErrorCode != "timeout" ||
		entry.ArgumentsDigest != "sha256:arguments" ||
		len(entry.Source) != 1 ||
		entry.Source[0].Source != "project_docs" ||
		entry.Source[0].Location != "project-a/doc-1" {
		t.Fatalf("compressed history entry = %+v, data = %s", entry, entry.Data)
	}
	if !reflect.DeepEqual(history, original) {
		t.Fatal("compression changed the original tool history")
	}
}

// TestEncodeGovernedToolHistoryDegradesBeforeOmitting 验证先移除旧 Data、再省略旧条目。
func TestEncodeGovernedToolHistoryDegradesBeforeOmitting(t *testing.T) {
	history := newContextGovernanceHistory()
	original := cloneContextGovernanceHistory(t, history)
	highPolicy := ContextPolicy{
		MaxHistoryTokens:    100000,
		MaxToolResultTokens: 10000,
		KeepLatestResults:   1,
	}
	fullEncoded, _, err := encodeGovernedToolHistory(history, highPolicy, "release-v1")
	if err != nil {
		t.Fatalf("encode full history error = %v", err)
	}
	degradedHistory := cloneContextGovernanceHistory(t, history)
	for index := 0; index < len(degradedHistory)-1; index++ {
		degradedHistory[index].Data = nil
	}
	degradedEncoded, _, err := encodeGovernedToolHistory(degradedHistory, highPolicy, "release-v1")
	if err != nil {
		t.Fatalf("encode degraded history error = %v", err)
	}
	criticalEncoded, _, err := encodeGovernedToolHistory(history[len(history)-1:], highPolicy, "release-v1")
	if err != nil {
		t.Fatalf("encode critical history error = %v", err)
	}
	fullTokens := estimateTokenCount(fullEncoded)
	degradedTokens := estimateTokenCount(degradedEncoded)
	criticalTokens := estimateTokenCount(criticalEncoded)
	if fullTokens <= degradedTokens || degradedTokens <= criticalTokens {
		t.Fatalf("unexpected reference sizes: full=%d degraded=%d critical=%d",
			fullTokens, degradedTokens, criticalTokens)
	}

	// 该预算需要移除全部非关键旧 Data 才能满足，但还不需要省略任何条目。
	degradedPolicy := highPolicy
	degradedPolicy.MaxHistoryTokens = degradedTokens
	encoded, report, err := encodeGovernedToolHistory(history, degradedPolicy, "release-v1")
	if err != nil {
		t.Fatalf("encode at degraded budget error = %v", err)
	}
	envelope := decodeContextGovernanceEnvelope(t, encoded)
	if !report.trimmed || envelope.OmittedEntries != 0 || len(envelope.Entries) != len(history) {
		t.Fatalf("degraded envelope = %+v, report = %+v", envelope, report)
	}
	for index := 0; index < len(history)-1; index++ {
		if len(envelope.Entries[index].Data) != 0 {
			t.Fatalf("old entry %d retained Data at degraded budget: %s", index, envelope.Entries[index].Data)
		}
	}
	if len(envelope.Entries[len(envelope.Entries)-1].Data) == 0 {
		t.Fatal("latest entry lost Data during old-entry degradation")
	}

	// 更小的预算在降级后继续从最旧条目省略，最终只留下最新关键证据。
	omittedPolicy := highPolicy
	omittedPolicy.MaxHistoryTokens = criticalTokens
	encoded, report, err = encodeGovernedToolHistory(history, omittedPolicy, "release-v1")
	if err != nil {
		t.Fatalf("encode at omitted budget error = %v", err)
	}
	envelope = decodeContextGovernanceEnvelope(t, encoded)
	if !report.trimmed ||
		envelope.OmittedEntries != len(history)-1 ||
		len(envelope.Entries) != 1 ||
		envelope.Entries[0].ToolName != "latest" {
		t.Fatalf("omitted envelope = %+v, report = %+v", envelope, report)
	}
	if !reflect.DeepEqual(history, original) {
		t.Fatal("degradation or omission changed the original tool history")
	}
}

// TestEncodeGovernedToolHistoryRetainsCriticalEntries 验证旧失败和最新结果不会被预算删除。
func TestEncodeGovernedToolHistoryRetainsCriticalEntries(t *testing.T) {
	history := newContextGovernanceHistory()
	history[0].Record.Status = tooling.ResultTimeout
	history[0].Record.ErrorCode = "timeout"
	original := cloneContextGovernanceHistory(t, history)
	encoded, report, err := encodeGovernedToolHistory(history, ContextPolicy{
		MaxHistoryTokens:    1,
		MaxToolResultTokens: 10000,
		KeepLatestResults:   1,
	}, "release-v1")
	if err != nil {
		t.Fatalf("encodeGovernedToolHistory() error = %v", err)
	}
	envelope := decodeContextGovernanceEnvelope(t, encoded)
	if !report.trimmed || envelope.OmittedEntries != 2 || len(envelope.Entries) != 2 {
		t.Fatalf("critical envelope = %+v, report = %+v", envelope, report)
	}
	failure := envelope.Entries[0]
	latest := envelope.Entries[1]
	if failure.ToolName != "old-0" ||
		failure.Status != string(tooling.ResultTimeout) ||
		failure.ErrorCode != "timeout" ||
		len(failure.Data) == 0 ||
		len(failure.Source) != 1 ||
		latest.ToolName != "latest" ||
		len(latest.Data) == 0 {
		t.Fatalf("critical entries = %+v", envelope.Entries)
	}
	if !reflect.DeepEqual(history, original) {
		t.Fatal("critical-entry retention changed the original history")
	}
}

// TestEncodeGovernedToolHistoryRejectsInvalidOldData 验证省略旧条目前仍校验其 JSON。
func TestEncodeGovernedToolHistoryRejectsInvalidOldData(t *testing.T) {
	history := newContextGovernanceHistory()
	history[0].Data = json.RawMessage(`{"invalid"`)
	_, _, err := encodeGovernedToolHistory(history, ContextPolicy{
		MaxHistoryTokens:    1,
		MaxToolResultTokens: 8,
		KeepLatestResults:   1,
	}, "release-v1")
	if !errors.Is(err, ErrToolHistoryEncodingFailed) {
		t.Fatalf("encodeGovernedToolHistory() error = %v, want ErrToolHistoryEncodingFailed", err)
	}
}

// TestRenderGovernedToolLoopPrompt 验证基线与实际 Prompt 同模板、同输入且 Token 口径准确。
func TestRenderGovernedToolLoopPrompt(t *testing.T) {
	history := []toolHistoryEntry{{
		Record: ToolCallRecord{
			ToolName:        "search_project_docs",
			ToolVersion:     "v1",
			Status:          tooling.ResultSuccess,
			ArgumentsDigest: "sha256:arguments",
		},
		Data: json.RawMessage(`{"text":"` + strings.Repeat("订单错误🙂", 100) + `"}`),
	}}
	originalHistory := cloneContextGovernanceHistory(t, history)
	state := toolLoopState{
		input:    RunInput{Query: "原始用户目标 <保留>"},
		snapshot: toolLoopSnapshot("release-v1"),
		analysis: structuredoutput.TaskAnalysis{
			Summary:    "原始任务分析 <保留>",
			Confidence: 0.9,
		},
		toolHistory: history,
	}
	originalSnapshot := state.snapshot
	runtime := &ToolLoopRuntime{contextPolicy: ContextPolicy{
		MaxHistoryTokens:    100000,
		MaxToolResultTokens: 8,
		KeepLatestResults:   1,
	}}
	const template = "query={{.Query}} analysis={{.Analysis.Summary}} history={{.ToolHistory}}"
	rendered, report, err := runtime.renderGovernedToolLoopPrompt(template, state)
	if err != nil {
		t.Fatalf("renderGovernedToolLoopPrompt() error = %v", err)
	}

	// 独立重建未治理基线，确认报告统计的是完整 Prompt 而不只是工具 Data。
	baselineEntries, err := projectToolHistory(history)
	if err != nil {
		t.Fatalf("projectToolHistory() error = %v", err)
	}
	baselineHistory, err := marshalToolHistoryEnvelope(
		baselineEntries,
		"context.v1:release-v1:h100000:r8:k1",
		0,
	)
	if err != nil {
		t.Fatalf("marshalToolHistoryEnvelope() error = %v", err)
	}
	baselinePrompt, err := renderPrompt(template, map[string]any{
		"Query":       state.input.Query,
		"Analysis":    state.analysis,
		"ToolHistory": baselineHistory,
	})
	if err != nil {
		t.Fatalf("renderPrompt() error = %v", err)
	}
	if report.contextVersion != "context.v1:release-v1:h100000:r8:k1" ||
		!report.trimmed ||
		report.tokensBefore != estimateTokenCount(baselinePrompt) ||
		report.tokensAfter != estimateTokenCount(rendered) ||
		report.tokensBefore <= report.tokensAfter {
		t.Fatalf("prompt governance report = %+v", report)
	}
	for _, expected := range []string{
		"原始用户目标 <保留>",
		"原始任务分析 <保留>",
		`"context_version":"context.v1:release-v1:h100000:r8:k1"`,
		`"compressed":true`,
	} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("governed prompt does not contain %q: %s", expected, rendered)
		}
	}
	if strings.Contains(rendered, strings.Repeat("订单错误🙂", 100)) {
		t.Fatal("governed prompt still contains the complete long tool result")
	}
	secondRendered, secondReport, err := runtime.renderGovernedToolLoopPrompt(template, state)
	if err != nil {
		t.Fatalf("second renderGovernedToolLoopPrompt() error = %v", err)
	}
	if rendered != secondRendered || report != secondReport {
		t.Fatal("Prompt rendering or Token report is not deterministic")
	}
	if !reflect.DeepEqual(history, originalHistory) || !reflect.DeepEqual(state.snapshot, originalSnapshot) {
		t.Fatal("Prompt governance modified the original history or ConfigSnapshot")
	}
}

// TestToolLoopPromptBuildsAlwaysStartFromOriginalHistory 验证决策和最终回答不会累积压缩结果。
func TestToolLoopPromptBuildsAlwaysStartFromOriginalHistory(t *testing.T) {
	snapshot := toolLoopSnapshot("v1")
	const template = "{{.Query}}|{{.Analysis.Summary}}|{{.ToolHistory}}"
	snapshot.Prompts[toolDecisionPromptName] = configregistry.PromptVersion{
		Name:     toolDecisionPromptName,
		Version:  "prompt.decision.v1",
		Template: template,
	}
	snapshot.Prompts[finalPromptName] = configregistry.PromptVersion{
		Name:     finalPromptName,
		Version:  "prompt.final.v1",
		Template: template,
	}
	history := newContextGovernanceHistory()
	originalHistory := cloneContextGovernanceHistory(t, history)
	runtime := &ToolLoopRuntime{contextPolicy: ContextPolicy{
		MaxHistoryTokens:    1,
		MaxToolResultTokens: 8,
		KeepLatestResults:   1,
	}}
	state := toolLoopState{
		input:       RunInput{Query: "查询订单文档"},
		snapshot:    snapshot,
		analysis:    structuredoutput.TaskAnalysis{Summary: "定位订单问题", Confidence: 0.9},
		toolHistory: history,
	}
	decisionState, err := runtime.renderToolDecisionPrompt(context.Background(), state)
	if err != nil {
		t.Fatalf("renderToolDecisionPrompt() error = %v", err)
	}
	finalState, err := runtime.renderToolLoopFinalPrompt(context.Background(), decisionState)
	if err != nil {
		t.Fatalf("renderToolLoopFinalPrompt() error = %v", err)
	}
	if decisionState.toolDecisionPrompt != finalState.finalPrompt {
		t.Fatalf("decision and final Prompt differ with identical inputs:\n%s\n%s",
			decisionState.toolDecisionPrompt, finalState.finalPrompt)
	}
	if !strings.Contains(finalState.finalPrompt, `"omitted_entries":3`) {
		t.Fatalf("final Prompt does not contain governed omission metadata: %s", finalState.finalPrompt)
	}
	if !reflect.DeepEqual(history, originalHistory) {
		t.Fatal("multiple Prompt builds accumulated compression into the original history")
	}
}

// TestToolLoopRunSendsGovernedPromptsWithoutExtraModelCalls 验证实际模型请求使用 envelope。
func TestToolLoopRunSendsGovernedPromptsWithoutExtraModelCalls(t *testing.T) {
	model := &fakeRuntimeModel{}
	cfg := newContextPolicyTestConfig(t)
	cfg.Model = model
	runtime, err := NewToolLoopRuntime(cfg)
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}
	if _, err := runtime.Run(context.Background(), RunInput{
		RequestID: "request-governed-prompts",
		TenantID:  "tenant-a",
		RunID:     "run-governed-prompts",
		Query:     "查询订单服务文档",
	}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if len(model.requests) != 3 {
		t.Fatalf("model requests = %d, want 3", len(model.requests))
	}
	if strings.Contains(model.requests[0].Messages[0].Content, "context_version") {
		t.Fatal("task analysis Prompt unexpectedly includes governed tool history")
	}
	for _, index := range []int{1, 2} {
		content := model.requests[index].Messages[0].Content
		if !strings.Contains(content, `"context_version":"context.v1:v1:h12000:r2560:k10"`) ||
			!strings.Contains(content, `"entries":[]`) {
			t.Fatalf("model request %d does not use governed envelope: %s", index, content)
		}
	}
}

// TestRecordContextGovernance 验证单次报告累计、裁剪计数和 RunStore 字段复制。
func TestRecordContextGovernance(t *testing.T) {
	const version = "context.v1:v1:h12000:r2560:k10"
	execution := &runtimeExecution{}
	recordContextGovernance(execution, contextGovernanceReport{
		contextVersion: version,
		tokensBefore:   120,
		tokensAfter:    120,
	})
	recordContextGovernance(execution, contextGovernanceReport{
		contextVersion: version,
		trimmed:        true,
		tokensBefore:   500,
		tokensAfter:    200,
	})

	got := snapshotRunObservation(execution)
	if got.ContextVersion != version ||
		got.ContextBuilds != 2 ||
		got.ContextTrimmed != 1 ||
		got.ContextTokensBefore != 620 ||
		got.ContextTokensAfter != 320 {
		t.Fatalf("context observation = %+v", got)
	}
	// 空执行对象是阶段级防御路径，不应引发 panic 或创建全局观测状态。
	recordContextGovernance(nil, contextGovernanceReport{})
}

// TestRecordContextGovernanceConcurrent 验证同一 Run 的并发报告不会丢失累计更新。
func TestRecordContextGovernanceConcurrent(t *testing.T) {
	const reportCount = 64
	execution := &runtimeExecution{}
	var wg sync.WaitGroup
	wg.Add(reportCount)
	for range reportCount {
		go func() {
			defer wg.Done()
			recordContextGovernance(execution, contextGovernanceReport{
				contextVersion: "context.v1:v1:h12000:r2560:k10",
				trimmed:        true,
				tokensBefore:   10,
				tokensAfter:    4,
			})
		}()
	}
	wg.Wait()

	got := snapshotRunObservation(execution)
	if got.ContextBuilds != reportCount ||
		got.ContextTrimmed != reportCount ||
		got.ContextTokensBefore != reportCount*10 ||
		got.ContextTokensAfter != reportCount*4 {
		t.Fatalf("concurrent context observation = %+v", got)
	}
}

// TestToolLoopRunRecordsContextObservation 验证无工具 Run 的版本、构建次数和双端字段一致性。
func TestToolLoopRunRecordsContextObservation(t *testing.T) {
	observer := &recordingObserver{}
	store := memory.NewMemoryStore()
	cfg := newContextPolicyTestConfig(t)
	cfg.Observer = observer
	cfg.RunStore = store
	runtime, err := NewToolLoopRuntime(cfg)
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}
	input := RunInput{
		RequestID: "request-context-version",
		TenantID:  "tenant-a",
		RunID:     "run-context-version",
		Query:     "查询订单服务文档",
	}
	if _, err := runtime.Run(context.Background(), input); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	const wantVersion = "context.v1:v1:h12000:r2560:k10"
	if len(observer.items) != 1 {
		t.Fatalf("observer items = %+v", observer.items)
	}
	observed := observer.items[0]
	if observed.ContextVersion != wantVersion ||
		observed.ContextBuilds != 2 ||
		observed.ContextTrimmed != 0 ||
		observed.ContextTokensBefore <= 0 ||
		observed.ContextTokensBefore != observed.ContextTokensAfter {
		t.Fatalf("observer context observation = %+v", observed)
	}
	record, err := store.GetRun(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if record.Observation.ContextVersion != wantVersion {
		t.Fatalf("RunStore context version = %q, want %q", record.Observation.ContextVersion, wantVersion)
	}
	assertContextObservationMatches(t, observed, record.Observation)
}

// TestToolLoopRunRecordsTrimmedContextWithoutSensitiveData 验证真实工具历史的指标和脱敏边界。
func TestToolLoopRunRecordsTrimmedContextWithoutSensitiveData(t *testing.T) {
	const secretMarker = "tool-secret-observer-runstore"
	data, err := json.Marshal(map[string]string{
		"text": secretMarker + strings.Repeat("订单🙂", 5000),
	})
	if err != nil {
		t.Fatalf("Marshal(tool data) error = %v", err)
	}
	tool := &toolLoopTestTool{
		definition: toolLoopExecutionDefinition(),
		result: tooling.ToolResult{
			Status: tooling.ResultSuccess,
			Data:   data,
			Source: []tooling.SourceRef{{Source: "project_docs", Location: "doc-1"}},
		},
	}
	model := &toolLoopIntegrationModel{}
	observer := &recordingObserver{}
	store := memory.NewMemoryStore()
	runtime := newContextGovernanceToolRuntime(t, model, tool, observer, store, ContextPolicy{})
	input := toolLoopRunInput("run-context-trimmed")
	if _, err := runtime.Run(context.Background(), input); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(model.requests) != 4 || len(observer.items) != 1 {
		t.Fatalf("model requests = %d, observer items = %d", len(model.requests), len(observer.items))
	}
	observed := observer.items[0]
	// 首次空历史决策不裁剪；工具后的第二轮决策和最终回答各压缩一次。
	if observed.ContextVersion != "context.v1:v1:h12000:r2560:k10" ||
		observed.ContextBuilds != 3 ||
		observed.ContextTrimmed != 2 ||
		observed.ContextTokensAfter <= 0 ||
		observed.ContextTokensBefore <= observed.ContextTokensAfter {
		t.Fatalf("trimmed context observation = %+v", observed)
	}
	// 本地上下文估算不能写入供应商真实 Usage；四次调用各贡献 3 个真实 Token。
	if observed.PromptTokens != 8 || observed.CompletionTokens != 4 || observed.TotalTokens != 12 {
		t.Fatalf("supplier usage was changed by context estimates: %+v", observed)
	}
	record, err := store.GetRun(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if record.Status != runstore.RunSucceeded {
		t.Fatalf("run status = %q, want succeeded", record.Status)
	}
	assertContextObservationMatches(t, observed, record.Observation)
	for name, value := range map[string]string{
		"observer": fmt.Sprintf("%+v", observed),
		"runstore": fmt.Sprintf("%+v", record),
	} {
		if strings.Contains(value, secretMarker) || strings.Contains(value, `"preview"`) {
			t.Fatalf("%s contains tool data or compression preview: %s", name, value)
		}
	}
}

// TestToolLoopFailedRunRetainsContextObservation 验证最终解析失败仍保留已完成的治理指标。
func TestToolLoopFailedRunRetainsContextObservation(t *testing.T) {
	model := &toolLoopFailureModel{
		decisionContent: `{"should_call":false}`,
		finalContent:    `{"status":`,
	}
	observer := &recordingObserver{}
	store := memory.NewMemoryStore()
	runtime := newContextGovernanceToolRuntime(t, model, nil, observer, store, ContextPolicy{})
	input := toolLoopRunInput("run-context-final-decode-failed")
	_, err := runtime.Run(context.Background(), input)
	if !errors.Is(err, ErrFinalDecodeFailed) {
		t.Fatalf("Run() error = %v, want ErrFinalDecodeFailed", err)
	}

	if len(observer.items) != 1 {
		t.Fatalf("observer items = %+v", observer.items)
	}
	observed := observer.items[0]
	if observed.ContextVersion != "context.v1:v1:h12000:r2560:k10" ||
		observed.ContextBuilds != 2 ||
		observed.ContextTrimmed != 0 ||
		observed.ContextTokensBefore <= 0 ||
		observed.ContextTokensBefore != observed.ContextTokensAfter ||
		observed.ErrorKind != "final_decode_error" {
		t.Fatalf("failed context observation = %+v", observed)
	}
	record, err := store.GetRun(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if record.Status != runstore.RunFailed || record.Observation.ErrorKind != "final_decode_error" {
		t.Fatalf("stored failed run = %+v", record)
	}
	assertContextObservationMatches(t, observed, record.Observation)
}

// TestToolLoopShortHistoryPromptsHaveStableTokenMetrics 验证非空短历史不发生无意义治理。
func TestToolLoopShortHistoryPromptsHaveStableTokenMetrics(t *testing.T) {
	snapshot := toolLoopSnapshot("v1")
	finalPrompt := snapshot.Prompts[finalPromptName]
	finalPrompt.Template = "final {{.Query}} {{.Analysis.Summary}} {{.ToolHistory}}"
	snapshot.Prompts[finalPromptName] = finalPrompt
	history := []toolHistoryEntry{{
		Record: ToolCallRecord{
			ToolName:        "search_project_docs",
			ToolVersion:     "v1",
			Status:          tooling.ResultSuccess,
			ArgumentsDigest: "sha256:short",
		},
		Data:   json.RawMessage(`{"hits":[{"id":"doc-1"}]}`),
		Source: []tooling.SourceRef{{Source: "project_docs", Location: "doc-1"}},
	}}
	original := cloneContextGovernanceHistory(t, history)
	execution := &runtimeExecution{}
	runtime := &ToolLoopRuntime{}
	state := toolLoopState{
		input:       RunInput{Query: "查询订单服务文档"},
		snapshot:    snapshot,
		analysis:    structuredoutput.TaskAnalysis{Summary: "定位订单问题", Confidence: 0.9},
		toolHistory: history,
		execution:   execution,
	}
	decisionState, err := runtime.renderToolDecisionPrompt(context.Background(), state)
	if err != nil {
		t.Fatalf("renderToolDecisionPrompt() error = %v", err)
	}
	finalState, err := runtime.renderToolLoopFinalPrompt(context.Background(), decisionState)
	if err != nil {
		t.Fatalf("renderToolLoopFinalPrompt() error = %v", err)
	}
	for _, prompt := range []string{decisionState.toolDecisionPrompt, finalState.finalPrompt} {
		if !strings.Contains(prompt, `"omitted_entries":0`) ||
			!strings.Contains(prompt, `"hits":[{"id":"doc-1"}]`) {
			t.Fatalf("short history Prompt was unexpectedly changed: %s", prompt)
		}
	}
	observed := snapshotRunObservation(execution)
	if observed.ContextBuilds != 2 ||
		observed.ContextTrimmed != 0 ||
		observed.ContextTokensBefore <= 0 ||
		observed.ContextTokensBefore != observed.ContextTokensAfter {
		t.Fatalf("short history context observation = %+v", observed)
	}
	if !reflect.DeepEqual(history, original) {
		t.Fatal("short history Prompt builds changed the original history")
	}
}

// TestToolLoopMultipleCallsGovernPromptWithoutChangingRunFacts 验证裁剪只影响模型输入投影。
func TestToolLoopMultipleCallsGovernPromptWithoutChangingRunFacts(t *testing.T) {
	model := &toolLoopSequenceModel{decisions: []string{
		`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"first"}}`,
		`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"second"}}`,
		`{"should_call":false}`,
	}}
	tool := &toolLoopTestTool{
		definition: toolLoopExecutionDefinition(),
		result: tooling.ToolResult{
			Status: tooling.ResultSuccess,
			Data:   json.RawMessage(`{"value":"short"}`),
			Source: []tooling.SourceRef{{Source: "project_docs", Location: "doc-1"}},
		},
	}
	observer := &recordingObserver{}
	store := memory.NewMemoryStore()
	runtime := newContextGovernanceToolRuntime(t, model, tool, observer, store, ContextPolicy{
		MaxHistoryTokens:    1,
		MaxToolResultTokens: 1000,
		KeepLatestResults:   1,
	})
	input := toolLoopRunInput("run-context-multiple-tools")
	output, err := runtime.Run(context.Background(), input)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	// 两次工具事实和三轮决策都必须保留，历史省略只发生在模型输入副本中。
	if len(output.ToolCalls) != 2 ||
		output.ToolDecisionRounds != 3 ||
		output.ToolCalls[0].ArgumentsDigest == "" ||
		output.ToolCalls[1].ArgumentsDigest == "" ||
		output.ToolCalls[0].ArgumentsDigest == output.ToolCalls[1].ArgumentsDigest {
		t.Fatalf("tool loop output = %+v", output)
	}
	if len(model.requests) != 5 || len(observer.items) != 1 {
		t.Fatalf("model requests = %d, observer items = %d", len(model.requests), len(observer.items))
	}
	if !strings.Contains(model.requests[2].Messages[0].Content, `"omitted_entries":0`) {
		t.Fatalf("first tool result was unexpectedly omitted: %s", model.requests[2].Messages[0].Content)
	}
	for _, index := range []int{3, 4} {
		if !strings.Contains(model.requests[index].Messages[0].Content, `"omitted_entries":1`) {
			t.Fatalf("request %d does not report omitted old history: %s", index, model.requests[index].Messages[0].Content)
		}
	}
	observed := observer.items[0]
	if observed.ContextVersion != "context.v1:v1:h1:r1000:k1" ||
		observed.ContextBuilds != 4 ||
		observed.ContextTrimmed != 2 ||
		observed.ContextTokensBefore <= observed.ContextTokensAfter {
		t.Fatalf("multiple-tool context observation = %+v", observed)
	}
	record, err := store.GetRun(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	assertContextObservationMatches(t, observed, record.Observation)
	events, err := store.ListEvents(context.Background(), input.RunID)
	if err != nil {
		t.Fatalf("ListEvents() error = %v", err)
	}
	toolCalls, toolResults := 0, 0
	for _, event := range events {
		switch event.Type {
		case runstore.EventToolCall:
			toolCalls++
		case runstore.EventToolResult:
			toolResults++
		}
	}
	if toolCalls != 2 || toolResults != 2 {
		t.Fatalf("stored tool facts = calls %d, results %d; events = %+v", toolCalls, toolResults, events)
	}
}

// TestToolLoopConcurrentRunsIsolateContextObservations 验证并发 Run 的版本、次数和 Token 相互隔离。
func TestToolLoopConcurrentRunsIsolateContextObservations(t *testing.T) {
	const runCount = 16
	observer := &recordingObserver{}
	store := memory.NewMemoryStore()
	cfg := newContextPolicyTestConfig(t)
	cfg.Observer = observer
	cfg.RunStore = store
	runtime, err := NewToolLoopRuntime(cfg)
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, runCount)
	wg.Add(runCount)
	for index := range runCount {
		go func() {
			defer wg.Done()
			input := RunInput{
				RequestID: fmt.Sprintf("request-context-concurrent-%d", index),
				TenantID:  "tenant-a",
				RunID:     fmt.Sprintf("run-context-concurrent-%d", index),
				Query:     "查询订单服务文档",
			}
			if _, err := runtime.Run(context.Background(), input); err != nil {
				errCh <- fmt.Errorf("Run(%s) error = %w", input.RunID, err)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	if len(observer.items) != runCount {
		t.Fatalf("observer items = %d, want %d", len(observer.items), runCount)
	}
	observedByRun := make(map[string]RunObservation, runCount)
	for _, observed := range observer.items {
		if _, exists := observedByRun[observed.RunID]; exists {
			t.Fatalf("duplicate observation for Run %q", observed.RunID)
		}
		observedByRun[observed.RunID] = observed
	}
	expectedTokensBefore, expectedTokensAfter := 0, 0
	for index := range runCount {
		runID := fmt.Sprintf("run-context-concurrent-%d", index)
		observed, ok := observedByRun[runID]
		if !ok {
			t.Fatalf("missing observation for Run %q", runID)
		}
		if observed.ContextVersion != "context.v1:v1:h12000:r2560:k10" ||
			observed.ContextBuilds != 2 ||
			observed.ContextTrimmed != 0 ||
			observed.ContextTokensBefore <= 0 ||
			observed.ContextTokensBefore != observed.ContextTokensAfter {
			t.Fatalf("Run %q context observation = %+v", runID, observed)
		}
		if index == 0 {
			expectedTokensBefore = observed.ContextTokensBefore
			expectedTokensAfter = observed.ContextTokensAfter
		} else if observed.ContextTokensBefore != expectedTokensBefore ||
			observed.ContextTokensAfter != expectedTokensAfter {
			t.Fatalf("Run %q Token report differs from other identical Runs: %+v", runID, observed)
		}
		record, err := store.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatalf("GetRun(%s) error = %v", runID, err)
		}
		if record.Status != runstore.RunSucceeded {
			t.Fatalf("Run %q status = %q", runID, record.Status)
		}
		assertContextObservationMatches(t, observed, record.Observation)
	}
}

// assertContextObservationMatches 验证 Observer 与 RunStore 的五个上下文字段完全一致。
func assertContextObservationMatches(
	t *testing.T,
	observed RunObservation,
	stored runstore.RunObservation,
) {
	t.Helper()
	if observed.ContextVersion != stored.ContextVersion ||
		observed.ContextBuilds != stored.ContextBuilds ||
		observed.ContextTrimmed != stored.ContextTrimmed ||
		observed.ContextTokensBefore != stored.ContextTokensBefore ||
		observed.ContextTokensAfter != stored.ContextTokensAfter {
		t.Fatalf("Observer context = %+v, RunStore context = %+v", observed, stored)
	}
}

// newContextGovernanceHistory 创建包含三条旧成功结果和一条最新结果的测试历史。
func newContextGovernanceHistory() []toolHistoryEntry {
	names := []string{"old-0", "old-1", "old-2", "latest"}
	history := make([]toolHistoryEntry, 0, len(names))
	for _, name := range names {
		history = append(history, toolHistoryEntry{
			Record: ToolCallRecord{
				ToolName:        name,
				ToolVersion:     "v1",
				Status:          tooling.ResultSuccess,
				ArgumentsDigest: "sha256:" + name,
			},
			Data: json.RawMessage(`{"value":"` + name + `-` + strings.Repeat("x", 80) + `"}`),
			Source: []tooling.SourceRef{{
				Source:   "source-" + name,
				Location: "location-" + name,
			}},
		})
	}
	return history
}

// cloneContextGovernanceHistory 通过 JSON 往返深拷贝测试历史及其 Data 和 Source。
func cloneContextGovernanceHistory(t *testing.T, history []toolHistoryEntry) []toolHistoryEntry {
	t.Helper()
	encoded, err := json.Marshal(history)
	if err != nil {
		t.Fatalf("Marshal(history) error = %v", err)
	}
	var cloned []toolHistoryEntry
	if err := json.Unmarshal(encoded, &cloned); err != nil {
		t.Fatalf("Unmarshal(history) error = %v", err)
	}
	return cloned
}

// decodeContextGovernanceEnvelope 将治理结果解码为测试可检查的受控投影。
func decodeContextGovernanceEnvelope(t *testing.T, encoded string) promptHistoryEnvelope {
	t.Helper()
	var envelope promptHistoryEnvelope
	if err := json.Unmarshal([]byte(encoded), &envelope); err != nil {
		t.Fatalf("Unmarshal(envelope) error = %v; encoded = %s", err, encoded)
	}
	return envelope
}

// newContextGovernanceToolRuntime 创建最终回答显式引用工具历史的集成测试 Runtime。
func newContextGovernanceToolRuntime(
	t *testing.T,
	model ModelCaller,
	tool tooling.Tool,
	observer Observer,
	store runstore.Store,
	policy ContextPolicy,
) *ToolLoopRuntime {
	t.Helper()
	configStore := configregistry.NewRegistry()
	snapshot := toolLoopSnapshot("v1")
	finalPrompt := snapshot.Prompts[finalPromptName]
	finalPrompt.Template = "final {{.Query}} {{.Analysis.Summary}} {{.ToolHistory}}"
	snapshot.Prompts[finalPromptName] = finalPrompt
	if err := configStore.Publish(snapshot); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := configStore.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(configStore, rollout.RolloutPolicy{
		CandidateVersion: "v1",
		Percentage:       100,
	})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	tools := toolregistry.NewToolRegistry()
	if tool != nil {
		if err := tools.Register(tool); err != nil {
			t.Fatalf("Register() error = %v", err)
		}
	}
	executor, err := runner.NewExecutor(tools)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	runtime, err := NewToolLoopRuntime(ToolLoopConfig{
		Registry:      configStore,
		Selector:      selector,
		Model:         model,
		Executor:      executor,
		Observer:      observer,
		RunStore:      store,
		ContextPolicy: policy,
		Authorizer: func(_ context.Context, input RunInput, decision structuredoutput.ToolDecision) (runner.ToolCallRequest, error) {
			return validToolLoopCallRequest(input, decision), nil
		},
	})
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}
	return runtime
}

// newContextPolicyTestConfig 创建满足工具循环安全边界的最小构造配置。
func newContextPolicyTestConfig(t *testing.T) ToolLoopConfig {
	t.Helper()
	registry := configregistry.NewRegistry()
	snapshot := toolLoopSnapshot("v1")
	finalPrompt := snapshot.Prompts[finalPromptName]
	finalPrompt.Template = "final {{.Query}} {{.Analysis.Summary}} {{.ToolHistory}}"
	snapshot.Prompts[finalPromptName] = finalPrompt
	if err := registry.Publish(snapshot); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	if err := registry.Activate("v1"); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}
	selector, err := rollout.NewSelector(registry, rollout.RolloutPolicy{
		CandidateVersion: "v1",
		Percentage:       100,
	})
	if err != nil {
		t.Fatalf("NewSelector() error = %v", err)
	}
	return ToolLoopConfig{
		Registry: registry,
		Selector: selector,
		Model:    &fakeRuntimeModel{},
		Executor: &runner.Executor{},
		Authorizer: func(context.Context, RunInput, structuredoutput.ToolDecision) (runner.ToolCallRequest, error) {
			return runner.ToolCallRequest{}, nil
		},
	}
}
