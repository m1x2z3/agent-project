package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"agent-project/internal/structuredoutput"
)

// digestToolLoopDecision 为“执行阶段 + 规范化模型决策”生成稳定摘要。
//
// encoding/json 会按排序后的键编码参数 Map，因此语义相同的决策不会因 Map
// 插入顺序不同而得到不同摘要。摘要只用于当前 Run 的循环检测，不保存原始参数。
func digestToolLoopDecision(
	stage string,
	decision structuredoutput.ToolDecision,
) (string, error) {
	encoded, err := json.Marshal(struct {
		Stage    string                        `json:"stage"`
		Decision structuredoutput.ToolDecision `json:"decision"`
	}{
		Stage:    stage,
		Decision: decision,
	})
	if err != nil {
		return "", fmt.Errorf("%w: marshal loop decision", ErrToolHistoryEncodingFailed)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
