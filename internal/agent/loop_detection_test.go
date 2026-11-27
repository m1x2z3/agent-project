package agent

import (
	"context"
	"errors"
	"testing"

	"agent-project/internal/structuredoutput"
	"agent-project/internal/tool/registry"
	"agent-project/internal/tool/runner"
	"agent-project/internal/tool/tooling"
)

// TestDigestToolLoopDecision 验证阶段隔离、参数规范化、非连续循环和非法参数。
func TestDigestToolLoopDecision(t *testing.T) {
	decisionA := structuredoutput.ToolDecision{
		ShouldCall: true,
		ToolName:   "tool_a",
		Arguments:  map[string]any{"query": "orders", "limit": 1},
	}
	decisionAReordered := structuredoutput.ToolDecision{
		ShouldCall: true,
		ToolName:   "tool_a",
		Arguments:  map[string]any{},
	}
	// 以不同插入顺序构造相同语义的参数，模拟不同模型响应的 Map 编码顺序。
	decisionAReordered.Arguments["limit"] = 1
	decisionAReordered.Arguments["query"] = "orders"
	decisionB := structuredoutput.ToolDecision{
		ShouldCall: true,
		ToolName:   "tool_b",
		Arguments:  map[string]any{"query": "orders"},
	}

	digestA, err := digestToolLoopDecision("tool_execution", decisionA)
	if err != nil {
		t.Fatalf("digestToolLoopDecision(A) error = %v", err)
	}
	digestAReordered, err := digestToolLoopDecision("tool_execution", decisionAReordered)
	if err != nil {
		t.Fatalf("digestToolLoopDecision(A reordered) error = %v", err)
	}
	digestB, err := digestToolLoopDecision("tool_execution", decisionB)
	if err != nil {
		t.Fatalf("digestToolLoopDecision(B) error = %v", err)
	}
	digestAAgain, err := digestToolLoopDecision("tool_execution", decisionA)
	if err != nil {
		t.Fatalf("digestToolLoopDecision(A again) error = %v", err)
	}
	if digestA != digestAReordered || digestA != digestAAgain {
		t.Fatalf("equivalent A decisions produced different digests: %q %q %q", digestA, digestAReordered, digestAAgain)
	}
	if digestA == digestB {
		t.Fatal("different decisions A and B produced the same digest")
	}
	// 这验证了 A -> B -> A 的首尾状态标识相同，而不是只比较相邻决策。
	if digestAAgain != digestA || digestAAgain == digestB {
		t.Fatalf("A -> B -> A digests = %q, %q, %q", digestA, digestB, digestAAgain)
	}

	otherStage, err := digestToolLoopDecision("tool_authorization", decisionA)
	if err != nil {
		t.Fatalf("digestToolLoopDecision(other stage) error = %v", err)
	}
	if otherStage == digestA {
		t.Fatal("different execution stages produced the same digest")
	}

	decisionA.Arguments["unsupported"] = func() {}
	_, err = digestToolLoopDecision("tool_execution", decisionA)
	if !errors.Is(err, ErrToolHistoryEncodingFailed) {
		t.Fatalf("unsupported decision error = %v, want ErrToolHistoryEncodingFailed", err)
	}
}

// newToolLoopForLoopDetection 创建可按轮次改变授权版本的循环检测测试实例。
func newToolLoopForLoopDetection(
	t *testing.T,
	decisions []string,
	versions []string,
) (*ToolLoopRuntime, *toolLoopTestTool, *recordingObserver) {
	t.Helper()
	model := &toolLoopSequenceModel{decisions: decisions}
	tool := &toolLoopTestTool{
		definition: toolLoopExecutionDefinition(),
		result: tooling.ToolResult{
			Status: tooling.ResultSuccess,
			Data:   []byte(`{"hits":[]}`),
		},
	}
	// 安全预检先于循环检测，因此变更版本的用例也必须让 v2 通过真实注册契约；
	// 否则预检会正确地先返回工具版本不存在，而不是进入本次要验证的循环判断。
	versionTwoDefinition := toolLoopExecutionDefinition()
	versionTwoDefinition.Version = "v2"
	versionTwoTool := &toolLoopTestTool{definition: versionTwoDefinition}
	toolRegistry := registry.NewToolRegistry()
	if err := toolRegistry.Register(tool); err != nil {
		t.Fatalf("Register(v1) error = %v", err)
	}
	if err := toolRegistry.Register(versionTwoTool); err != nil {
		t.Fatalf("Register(v2) error = %v", err)
	}
	executor, err := runner.NewExecutor(toolRegistry)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	observer := &recordingObserver{}
	authorizerCalls := 0
	cfg := newContextPolicyTestConfig(t)
	cfg.Model = model
	cfg.Executor = executor
	cfg.Observer = observer
	cfg.Authorizer = func(
		_ context.Context,
		input RunInput,
		decision structuredoutput.ToolDecision,
	) (runner.ToolCallRequest, error) {
		version := "v1"
		if authorizerCalls < len(versions) && versions[authorizerCalls] != "" {
			version = versions[authorizerCalls]
		}
		authorizerCalls++
		request := validToolLoopCallRequest(input, decision)
		request.ToolVersion = version
		request.CapabilityToken.ToolVersion = version
		return request, nil
	}
	runtime, err := NewToolLoopRuntime(cfg)
	if err != nil {
		t.Fatalf("NewToolLoopRuntime() error = %v", err)
	}
	return runtime, tool, observer
}

// TestToolLoopExistingDuplicateDetectionCoversNonConsecutiveCall 验证已有规则已覆盖同版本 A -> B -> A。
func TestToolLoopExistingDuplicateDetectionCoversNonConsecutiveCall(t *testing.T) {
	decisions := []string{
		`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"a"}}`,
		`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"b"}}`,
		`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"a"}}`,
	}
	runtime, tool, _ := newToolLoopForLoopDetection(t, decisions, nil)

	_, err := runtime.Run(context.Background(), toolLoopRunInput("run-existing-non-consecutive-duplicate"))
	if !errors.Is(err, ErrDuplicateToolCall) {
		t.Fatalf("Run() error = %v, want existing ErrDuplicateToolCall", err)
	}
	if errors.Is(err, ErrLoopDetected) {
		t.Fatalf("Run() error = %v, exact duplicate must retain its original classification", err)
	}
	if tool.executeCalls != 2 {
		t.Fatalf("tool execute calls = %d, want only A and B", tool.executeCalls)
	}
	if len(runtime.model.(*toolLoopSequenceModel).requests) != 4 {
		t.Fatalf("model calls = %d, want analysis and three decisions", len(runtime.model.(*toolLoopSequenceModel).requests))
	}
}

// TestToolLoopLoopDetectionCoversChangedVersion 验证通用检测补足精确规则之外的重复模型状态。
func TestToolLoopLoopDetectionCoversChangedVersion(t *testing.T) {
	decisions := []string{
		`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"a"}}`,
		`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"b"}}`,
		`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"a"}}`,
	}
	// 第三次模型决策与第一次相同，但可信授权方提供了不同版本，精确重复检查不会命中。
	runtime, tool, observer := newToolLoopForLoopDetection(t, decisions, []string{"v1", "v1", "v2"})

	_, err := runtime.Run(context.Background(), toolLoopRunInput("run-no-progress-changed-version"))
	if !errors.Is(err, ErrLoopDetected) {
		t.Fatalf("Run() error = %v, want ErrLoopDetected", err)
	}
	if errors.Is(err, ErrDuplicateToolCall) {
		t.Fatalf("Run() error = %v, changed version must use generic loop detection", err)
	}
	if tool.executeCalls != 2 {
		t.Fatalf("tool execute calls = %d, want third decision stopped before Executor", tool.executeCalls)
	}
	if len(observer.items) != 1 || observer.items[0].Success || observer.items[0].ErrorKind != "loop_detected" {
		t.Fatalf("loop observation = %+v, want one loop_detected failure", observer.items)
	}
}

// TestToolLoopLoopDetectionAllowsNewArguments 验证持续产生新参数的正常调用不会误判。
func TestToolLoopLoopDetectionAllowsNewArguments(t *testing.T) {
	decisions := []string{
		`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"a"}}`,
		`{"should_call":true,"tool_name":"tool_loop_test","arguments":{"query":"b"}}`,
		`{"should_call":false}`,
	}
	runtime, tool, _ := newToolLoopForLoopDetection(t, decisions, nil)

	output, err := runtime.Run(context.Background(), toolLoopRunInput("run-no-progress-different-arguments"))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if tool.executeCalls != 2 || len(output.ToolCalls) != 2 || output.FinalAnswer.Answer != "已完成" {
		t.Fatalf("tool calls = %d, output = %+v, want two successful new calls", tool.executeCalls, output)
	}
}
