package model

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

func TestMergeURLValuesPreservesGeneratedQuery(t *testing.T) {
	got := mergeURLValues(
		url.Values{"alt": {"sse"}},
		url.Values{"alt": {"client"}, "trace": {"1"}},
	)
	if got.Get("alt") != "sse" || got.Get("trace") != "1" {
		t.Fatalf("unexpected merged query: %v", got)
	}
}

func TestMergeInternalResponseUsesTerminalUsage(t *testing.T) {
	initial := &llm.Response{Usage: &llm.Usage{PromptTokens: 1, TotalTokens: 2}}
	terminal := &llm.Response{Usage: &llm.Usage{PromptTokens: 10, TotalTokens: 20}}
	merged := mergeAxonChunks([]*llm.Response{initial, terminal})
	if merged == nil || merged.Usage == nil {
		t.Fatal("expected merged usage")
	}
	if merged.Usage.PromptTokens != 10 || merged.Usage.TotalTokens != 20 {
		t.Fatalf("expected terminal usage to win, got %+v", merged.Usage)
	}
}

func TestTransformResponseRejectsNilBody(t *testing.T) {
	_, err := TransformResponse(context.Background(), nil, &http.Response{})
	if err == nil || !strings.Contains(err.Error(), "response body is nil") {
		t.Fatalf("expected nil response body error, got %v", err)
	}
}

type axonBridgeOutboundStub struct {
	transformer.Outbound
	format        llm.APIFormat
	streamErr     error
	streamFactory func(context.Context, streams.Stream[*httpclient.StreamEvent]) streams.Stream[*llm.Response]
}

func (s *axonBridgeOutboundStub) APIFormat() llm.APIFormat { return s.format }

func (s *axonBridgeOutboundStub) TransformStream(ctx context.Context, _ *httpclient.Request, input streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*llm.Response], error) {
	if s.streamErr != nil {
		return nil, s.streamErr
	}
	return s.streamFactory(ctx, input), nil
}

type feederResponseStream struct {
	input   streams.Stream[*httpclient.StreamEvent]
	current *llm.Response
}

func (s *feederResponseStream) Next() bool {
	if !s.input.Next() {
		return false
	}
	event := s.input.Current()
	if event == nil {
		s.current = nil
		return true
	}
	if string(event.Data) == "[DONE]" {
		s.current = llm.DoneResponse
		return true
	}
	text := string(event.Data)
	s.current = &llm.Response{
		ID:     "stub-response",
		Model:  "stub-model",
		Object: "chat.completion.chunk",
		Choices: []llm.Choice{{
			Index: 0,
			Delta: &llm.Message{Content: llm.MessageContent{Content: &text}},
		}},
	}
	return true
}

func (s *feederResponseStream) Current() *llm.Response { return s.current }
func (s *feederResponseStream) Err() error             { return nil }
func (s *feederResponseStream) Close() error           { return nil }

func TestAxonStreamBridgeFeedsEventsAndTerminatesOnDone(t *testing.T) {
	stub := &axonBridgeOutboundStub{
		format: llm.APIFormatOpenAIChatCompletion,
		streamFactory: func(_ context.Context, input streams.Stream[*httpclient.StreamEvent]) streams.Stream[*llm.Response] {
			return &feederResponseStream{input: input}
		},
	}
	bridge := NewAxonStreamBridge(stub)

	response, err := bridge.Feed(context.Background(), []byte(`{"type":"text","data":"hello"}`))
	if err != nil {
		t.Fatalf("Feed(): %v", err)
	}
	if response == nil || len(response.Choices) != 1 || response.Choices[0].Delta == nil || response.Choices[0].Delta.Content.Content == nil || *response.Choices[0].Delta.Content.Content == "" {
		t.Fatalf("expected transformed response, got %+v", response)
	}

	done, err := bridge.Feed(context.Background(), []byte("[DONE]"))
	if err != nil {
		t.Fatalf("Feed([DONE]): %v", err)
	}
	if done == nil || done.Object != "[DONE]" {
		t.Fatalf("expected terminal response, got %+v", done)
	}
	select {
	case <-bridge.done:
	case <-time.After(time.Second):
		t.Fatal("expected AxonHub stream goroutine to terminate after [DONE]")
	}
}

func TestAxonStreamBridgeRejectsNilStream(t *testing.T) {
	bridge := NewAxonStreamBridge(&axonBridgeOutboundStub{format: llm.APIFormatOpenAIChatCompletion, streamFactory: func(context.Context, streams.Stream[*httpclient.StreamEvent]) streams.Stream[*llm.Response] {
		return nil
	}})
	_, err := bridge.Feed(context.Background(), []byte(`{"type":"text"}`))
	if err == nil || !strings.Contains(err.Error(), "nil stream") {
		t.Fatalf("expected nil stream error, got %v", err)
	}
	select {
	case <-bridge.done:
	case <-time.After(time.Second):
		t.Fatal("expected done to close for nil stream")
	}
}

func TestAxonStreamBridgePropagatesTransformStreamError(t *testing.T) {
	want := errors.New("stream setup failed")
	bridge := NewAxonStreamBridge(&axonBridgeOutboundStub{format: llm.APIFormatOpenAIChatCompletion, streamErr: want})
	_, err := bridge.Feed(context.Background(), []byte(`{"type":"text"}`))
	if err == nil || !strings.Contains(err.Error(), want.Error()) {
		t.Fatalf("expected setup error, got %v", err)
	}
}

type contextBlockingResponseStream struct {
	ctx     context.Context
	started chan struct{}
	err     error
}

func (s *contextBlockingResponseStream) Next() bool {
	select {
	case <-s.started:
	default:
		close(s.started)
	}
	<-s.ctx.Done()
	s.err = s.ctx.Err()
	return false
}
func (s *contextBlockingResponseStream) Current() *llm.Response { return nil }
func (s *contextBlockingResponseStream) Err() error             { return s.err }
func (s *contextBlockingResponseStream) Close() error           { return nil }

func TestAxonStreamBridgeHonorsContextCancellation(t *testing.T) {
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge := NewAxonStreamBridge(&axonBridgeOutboundStub{
		format: llm.APIFormatOpenAIChatCompletion,
		streamFactory: func(ctx context.Context, _ streams.Stream[*httpclient.StreamEvent]) streams.Stream[*llm.Response] {
			return &contextBlockingResponseStream{ctx: ctx, started: started}
		},
	})

	type result struct {
		response *InternalLLMResponse
		err      error
	}
	resultCh := make(chan result, 1)
	go func() {
		response, err := bridge.Feed(ctx, []byte(`{"type":"text"}`))
		resultCh <- result{response: response, err: err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream transformer did not start")
	}
	cancel()

	select {
	case got := <-resultCh:
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("expected context cancellation, got response=%+v err=%v", got.response, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("Feed did not return after context cancellation")
	}
}
