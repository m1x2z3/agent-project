package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudwego/eino/components/model"
	"github.com/cloudwego/eino/schema"
)

func TestPipelineRunsChainAndGraph(t *testing.T) {
	pipeline, err := NewPipeline(context.Background(), NewDemoChatModel())
	if err != nil {
		t.Fatalf("NewPipeline() error = %v", err)
	}

	input := Input{Query: "如何定位订单服务错误"}
	chainOutput, err := pipeline.RunChain(context.Background(), input)
	if err != nil {
		t.Fatalf("RunChain() error = %v", err)
	}
	graphOutput, err := pipeline.RunGraph(context.Background(), input)
	if err != nil {
		t.Fatalf("RunGraph() error = %v", err)
	}
	if chainOutput.Answer == "" || graphOutput.Answer == "" {
		t.Fatalf("empty outputs: chain=%+v graph=%+v", chainOutput, graphOutput)
	}
	if chainOutput != graphOutput {
		t.Fatalf("chain and graph outputs differ: chain=%+v graph=%+v", chainOutput, graphOutput)
	}
	if !strings.Contains(chainOutput.Answer, input.Query) {
		t.Fatalf("answer = %q, want query %q", chainOutput.Answer, input.Query)
	}
}

func TestPipelineRejectsEmptyQueryBeforeModel(t *testing.T) {
	model := &recordingChatModel{}
	pipeline, err := NewPipeline(context.Background(), model)
	if err != nil {
		t.Fatalf("NewPipeline() error = %v", err)
	}

	_, err = pipeline.Run(context.Background(), Input{Query: "  "})
	if !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("error = %v, want ErrEmptyQuery", err)
	}
	if model.generateCalls != 0 {
		t.Fatalf("Generate calls = %d, want 0", model.generateCalls)
	}
}

func TestPipelinePropagatesModelError(t *testing.T) {
	expected := errors.New("model unavailable")
	pipeline, err := NewPipeline(context.Background(), &recordingChatModel{err: expected})
	if err != nil {
		t.Fatalf("NewPipeline() error = %v", err)
	}

	_, err = pipeline.Run(context.Background(), Input{Query: "test"})
	if !errors.Is(err, expected) {
		t.Fatalf("error = %v, want wrapped model error", err)
	}
}

func TestPipelinePropagatesContextCancellationToModel(t *testing.T) {
	model := &recordingChatModel{waitForContext: true, started: make(chan struct{})}
	pipeline, err := NewPipeline(context.Background(), model)
	if err != nil {
		t.Fatalf("NewPipeline() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() {
		_, runErr := pipeline.Run(ctx, Input{Query: "cancel me"})
		resultCh <- runErr
	}()

	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("model was not invoked")
	}
	cancel()

	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("pipeline did not stop after context cancellation")
	}
}

func TestBuildRejectsNilModel(t *testing.T) {
	if _, err := BuildChain(context.Background(), nil); !errors.Is(err, ErrNilChatModel) {
		t.Fatalf("BuildChain() error = %v, want ErrNilChatModel", err)
	}
	if _, err := BuildGraph(context.Background(), nil); !errors.Is(err, ErrNilChatModel) {
		t.Fatalf("BuildGraph() error = %v, want ErrNilChatModel", err)
	}
	if _, err := NewPipeline(context.Background(), nil); !errors.Is(err, ErrNilChatModel) {
		t.Fatalf("NewPipeline() error = %v, want ErrNilChatModel", err)
	}
}

type recordingChatModel struct {
	err            error
	waitForContext bool
	started        chan struct{}
	startOnce      sync.Once
	generateCalls  int
}

func (m *recordingChatModel) Generate(ctx context.Context, _ []*schema.Message, _ ...model.Option) (*schema.Message, error) {
	m.generateCalls++
	if m.started != nil {
		m.startOnce.Do(func() {
			close(m.started)
		})
	}
	if m.waitForContext {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if m.err != nil {
		return nil, m.err
	}
	return schema.AssistantMessage("recorded", nil), nil
}

func (m *recordingChatModel) Stream(ctx context.Context, messages []*schema.Message, opts ...model.Option) (*schema.StreamReader[*schema.Message], error) {
	message, err := m.Generate(ctx, messages, opts...)
	if err != nil {
		return nil, err
	}
	return schema.StreamReaderFromArray([]*schema.Message{message}), nil
}
