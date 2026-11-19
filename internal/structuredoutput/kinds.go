package structuredoutput

// OutputKind 标识模型响应对应的业务结果类型。
type OutputKind string

const (
	OutputTaskAnalysis OutputKind = "task_analysis"
	OutputToolDecision OutputKind = "tool_decision"
	OutputFinalAnswer  OutputKind = "final_answer"
)

const (
	TaskAnalysisSchemaVersion = "task_analysis.v1"
	ToolDecisionSchemaVersion = "tool_decision.v1"
	FinalAnswerSchemaVersion  = "final_answer.v1"
)

const (
	FinalAnswerStatusAnswered = "answered"
	FinalAnswerStatusRefused  = "refused"
)
