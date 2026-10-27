package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bestruirui/octopus/internal/conf"
	"github.com/bestruirui/octopus/internal/helper"
	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/relay/stream"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/bestruirui/octopus/internal/utils/log"
)

const maxAgentContinuations = 2
const agentContinuationReminder = "Continue the unfinished task from your preceding message by performing the next concrete action with the available tools. Do not repeat actions already performed. Do not stop after merely announcing another action. If the user's task is actually complete, report the result; if user input is indispensable, ask a specific question."

func (ra *relayAttempt) sendAgentContinuation(req *http.Request) (*http.Response, error) {
	// The client has already received tokens. Do not start another first-token
	// budget whose timer can no longer be stopped by the processor's callback.
	client, err := helper.ChannelHTTPClientWithContext(req.Context(), ra.channel)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}

var agentTransitionAction = regexp.MustCompile(`(?i)(现在|接下来|下一步|下面|然后|我先|我来|我会|我将|开始|继续|换个|换一种|改用|改成|先|补上|补充|写|运行|执行|做).*(测试|验证|检查|修复|实现|修改|写|编辑|读|运行|执行|查看|拉取|查询|调用|格式|部署|构建|安装|补|固化|配置|启用|调整|接入|处理|尝试)|\b(i['’]ll|i will|let me|next i|now i).*(test|check|fix|implement|edit|write|read|run|execute|inspect|fetch|call|change|try|deploy|build|install)`)
var agentAwaitingUser = regexp.MustCompile(`(?i)(请选择|请告诉|请你|请提供|请确认|需要你|等你|等待你|你是否|要不要|是否需要|可以吗|确认后|待确认|if you|would you|let me know|once you|please confirm|please provide|please select|need your)`)

// Deliberately conservative: this is an opt-in workaround for a short action
// announcement, not a task-completion oracle for arbitrary model replies.
func prematureAgentAnnouncement(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > 600 || strings.ContainsAny(text, "?？") || strings.Contains(text, "```") || agentAwaitingUser.MatchString(text) {
		return false
	}
	tail := text
	if index := strings.LastIndexAny(tail, "\n。！!"); index >= 0 && index < len(tail)-1 {
		_, size := utf8.DecodeRuneInString(tail[index:])
		tail = strings.TrimSpace(tail[index+size:])
	}
	// A colon with no following action/content is the observed failure mode.
	return (strings.HasSuffix(tail, ":") || strings.HasSuffix(tail, "：")) && agentTransitionAction.MatchString(tail)
}

func (ra *relayAttempt) agentContinuationEnabled() bool {
	req := ra.internalRequest
	if req == nil || ra.channel.Type != outbound.OutboundTypeOpenAIChat || req.RawAPIFormat != model.APIFormatOpenAIResponse || len(req.Tools) == 0 ||
		!slices.Contains(conf.AppConfig.Relay.AgentAutoContinueChannelIDs, ra.channel.ID) {
		return false
	}
	for _, message := range req.Messages {
		if (message.Role == "developer" || message.Role == "system") && message.Content.Content != nil &&
			strings.Contains(*message.Content.Content, "operating inside pi") {
			return true
		}
	}
	return false
}

type agentContinuationSource struct {
	ctx           context.Context
	cancel        context.CancelFunc
	mu            sync.Mutex // protects the replaceable source and request records against Close
	source        stream.StreamSource
	closed        bool
	records       []dbmodel.ChannelAttempt
	request       *http.Request
	payload       map[string]json.RawMessage
	send          func(*http.Request) (*http.Response, error)
	queue         [][]byte
	id            string
	text          strings.Builder
	reasoning     strings.Builder
	canRecover    bool
	hasTools      bool
	holding       bool
	recoveries    int
	previousUsage map[string]interface{}
	turnUsage     map[string]interface{}
	usageFlushed  bool
	finished      bool
	latestStarted time.Time
	pendingErr    error
}

func (s *agentContinuationSource) finishAttempt(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.records) == 0 {
		return
	}
	record := &s.records[len(s.records)-1]
	record.Duration = int(time.Since(s.latestStarted).Milliseconds())
	if err != nil {
		record.Status = dbmodel.AttemptFailed
		record.Msg = err.Error()
	} else if s.holding {
		record.Status = dbmodel.AttemptFailed
		record.Msg = "premature agent stop; automatic continuation required"
	} else if s.finished {
		record.Status = dbmodel.AttemptSuccess
		record.Msg = "automatic continuation after premature agent stop"
	}
}

func newAgentContinuationSource(ctx context.Context, response *http.Response, request *http.Request, send func(*http.Request) (*http.Response, error)) (*agentContinuationSource, error) {
	body, err := readOutboundRequestBody(request)
	if err != nil {
		return nil, err
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if bytes.Equal(bytes.TrimSpace(payload["tool_choice"]), []byte(`"none"`)) {
		return nil, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	return &agentContinuationSource{ctx: ctx, cancel: cancel, source: stream.NewSSESource(response.Body, maxSSEEventSize), request: request, payload: payload, send: send, canRecover: true}, nil
}

func (s *agentContinuationSource) Close() error {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	source := s.source
	s.mu.Unlock()
	return source.Close()
}

func (s *agentContinuationSource) attempts() []dbmodel.ChannelAttempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]dbmodel.ChannelAttempt(nil), s.records...)
}

func (s *agentContinuationSource) ReadEvent(ctx context.Context) ([]byte, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		if s.pendingErr != nil {
			return nil, s.pendingErr
		}
		if len(s.queue) > 0 {
			data := s.queue[0]
			s.queue = s.queue[1:]
			return data, nil
		}
		s.mu.Lock()
		source := s.source
		s.mu.Unlock()
		data, err := source.ReadEvent(ctx)
		if err != nil && !errors.Is(err, io.EOF) {
			s.finishAttempt(err)
			return nil, err
		}
		if errors.Is(err, io.EOF) || bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			s.finishAttempt(nil)
			if !s.holding {
				if s.recoveries > 0 && !s.usageFlushed && s.turnUsage == nil && len(s.previousUsage) > 0 {
					s.usageFlushed = true
					if err == nil {
						s.queue = append(s.queue, data)
					}
					return json.Marshal(map[string]interface{}{"id": s.id, "object": "chat.completion.chunk", "choices": []interface{}{}, "usage": s.previousUsage})
				}
				return data, err
			}
			if err := s.recover(); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil, err
				}
				s.pendingErr = err
				return json.Marshal(map[string]interface{}{"error": map[string]string{"type": "agent_continuation_failed", "code": "agent_continuation_failed", "message": err.Error()}})
			}
			continue
		}
		var event map[string]json.RawMessage
		if json.Unmarshal(data, &event) != nil {
			return data, nil
		}
		if s.id == "" {
			_ = json.Unmarshal(event["id"], &s.id)
		}
		if s.recoveries > 0 && s.id != "" {
			event["id"], _ = json.Marshal(s.id)
		}
		if raw := event["usage"]; len(raw) > 0 && string(raw) != "null" {
			var usage map[string]interface{}
			if json.Unmarshal(raw, &usage) == nil {
				s.turnUsage = usage
				if s.previousUsage != nil {
					event["usage"], _ = json.Marshal(addAgentUsage(s.previousUsage, usage))
				}
			}
		}
		var choices []map[string]json.RawMessage
		if json.Unmarshal(event["choices"], &choices) == nil {
			if len(choices) > 1 {
				s.canRecover = false
			}
			for _, choice := range choices {
				if string(choice["index"]) != "0" {
					s.canRecover = false
				}
				var delta struct {
					Content      string            `json:"content"`
					Reasoning    string            `json:"reasoning_content"`
					ToolCalls    []json.RawMessage `json:"tool_calls"`
					FunctionCall json.RawMessage   `json:"function_call"`
				}
				if json.Unmarshal(choice["delta"], &delta) != nil {
					s.canRecover = false
					continue
				}
				if s.text.Len()+len(delta.Content) <= 8192 {
					s.text.WriteString(delta.Content)
				} else {
					s.canRecover = false
				}
				if s.reasoning.Len()+len(delta.Reasoning) <= 128*1024 {
					s.reasoning.WriteString(delta.Reasoning)
				} else {
					s.canRecover = false
				}
				if len(delta.ToolCalls) > 0 || (len(delta.FunctionCall) > 0 && string(delta.FunctionCall) != "null") {
					s.hasTools = true
				}
				var reason string
				_ = json.Unmarshal(choice["finish_reason"], &reason)
				if reason != "" {
					s.finished = true
				}
				if reason == "stop" && s.canRecover && !s.hasTools && prematureAgentAnnouncement(s.text.String()) {
					s.holding = true
					choice["finish_reason"] = json.RawMessage("null")
				}
			}
			if s.holding {
				event["choices"], _ = json.Marshal(choices)
			}
		}
		// Withhold usage for a recovered turn. Its cumulative usage is emitted
		// with the eventual final turn, never as an intermediate completion.
		if s.holding {
			delete(event, "usage")
		}
		if s.recoveries > 0 || s.holding {
			data, err = json.Marshal(event)
		}
		return data, err
	}
}

func addAgentUsage(a, b map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(a)+len(b))
	for key, value := range a {
		result[key] = value
	}
	for key, value := range b {
		if nested, ok := value.(map[string]interface{}); ok {
			previous, _ := result[key].(map[string]interface{})
			result[key] = addAgentUsage(previous, nested)
		} else if number, ok := value.(float64); ok {
			previous, _ := result[key].(float64)
			result[key] = previous + number
		} else {
			result[key] = value
		}
	}
	return result
}

func (s *agentContinuationSource) recover() error {
	if s.recoveries >= maxAgentContinuations {
		return fmt.Errorf("agent stopped before performing its announced action after %d automatic continuations", s.recoveries)
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	var messages []json.RawMessage
	if err := json.Unmarshal(s.payload["messages"], &messages); err != nil {
		return err
	}
	assistant := map[string]string{"role": "assistant", "content": s.text.String()}
	if s.reasoning.Len() > 0 {
		assistant["reasoning_content"] = s.reasoning.String()
	}
	message, _ := json.Marshal(assistant)
	reminder, _ := json.Marshal(map[string]string{"role": "user", "content": agentContinuationReminder})
	messages = append(messages, message, reminder)
	s.payload["messages"], _ = json.Marshal(messages)
	body, err := json.Marshal(s.payload)
	if err != nil {
		return err
	}
	next := s.request.Clone(s.ctx)
	next.Body = nil
	setPromptCacheCompatibilityBody(next, body)
	s.mu.Lock()
	old := s.source
	index := len(s.records)
	s.records = append(s.records, dbmodel.ChannelAttempt{Status: dbmodel.AttemptFailed, Msg: "automatic agent continuation canceled"})
	s.mu.Unlock()
	_ = old.Close()
	started := time.Now()
	s.latestStarted = started
	response, err := s.send(next)
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	if err == nil && (response == nil || status < 200 || status >= 300) {
		err = fmt.Errorf("agent continuation upstream HTTP status %d", status)
	}
	if err == nil && !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		err = fmt.Errorf("agent continuation upstream returned non-SSE content")
	}
	s.mu.Lock()
	s.records[index].Duration = int(time.Since(started).Milliseconds())
	if err != nil {
		s.records[index].Msg = err.Error()
	}
	if err == nil && !s.closed {
		s.source = stream.NewSSESource(response.Body, maxSSEEventSize)
	} else if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	closed := s.closed
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if closed {
		return context.Canceled
	}
	s.previousUsage = addAgentUsage(s.previousUsage, s.turnUsage)
	s.turnUsage = nil
	s.recoveries++
	s.text.Reset()
	s.reasoning.Reset()
	s.holding = false
	s.hasTools = false
	s.canRecover = true
	s.finished = false
	log.Warnf("agent premature stop recovered: continuation=%d/%d", s.recoveries, maxAgentContinuations)
	return nil
}
