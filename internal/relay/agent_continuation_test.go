package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

func agentTestChunk(id, text, reason string) string {
	return fmt.Sprintf("data: {\"id\":%q,\"object\":\"chat.completion.chunk\",\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":%s}]}\n\n", id, text, reason)
}

func agentTestRequest(t *testing.T) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodPost, "http://fixture.invalid/v1/chat/completions", strings.NewReader(`{"model":"test","stream":true,"tools":[{"type":"function","function":{"name":"bash","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"fix and test"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPrematureAgentAnnouncement(t *testing.T) {
	for _, text := range []string{"拉取成功，后台下拉会显示这两张卡，默认关闭。现在补测试：", "文档说明支持 JSON 格式。换个格式：", "平台名片已经能拉到了。补上测试：:", "补充测试：", "运行测试：", "平台会重写 `customer_channel`。现在把结构固化，让后台开关真正能用：", "浏览器进不去。先试最后一个思路：从历史会话读取完整 CARD，原样复用这个结构：", "四种简化格式都被拒绝。现在尝试完整卡片结构：从历史会话中读出一条平台名片，原样复用它：", "接下来运行测试：", "I'll run the tests now:"} {
		if !prematureAgentAnnouncement(text) {
			t.Errorf("missed action announcement %q", text)
		}
	}
	for _, text := range []string{"测试全部通过，任务已完成。", "请选择接下来运行的测试：", "请确认后我会继续测试：", "需要你提供 JSON 格式：", "下一步是否需要补测试？", "JSON 格式如下：", "```json\n{}\n```", "下一步建议运行测试。", strings.Repeat("a", 601) + "现在补测试："} {
		if prematureAgentAnnouncement(text) {
			t.Errorf("continued a completed answer or user question %q", text)
		}
	}
}

func TestAgentContinuationKeepsOneResponseAndAggregatesUsage(t *testing.T) {
	first := agentTestChunk("initial", "文档说明支持 JSON 格式。换个格式：", `"stop"`) + `data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":10,"total_tokens":110,"prompt_tokens_details":{"cached_tokens":80}}}` + "\n\ndata: [DONE]\n\n"
	second := `data: {"id":"followup","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_test","type":"function","function":{"name":"bash","arguments":"{}"}}]},"finish_reason":null}]}` + "\n\n" + agentTestChunk("followup", "", `"tool_calls"`) + `data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":20,"total_tokens":140,"prompt_tokens_details":{"cached_tokens":100}}}` + "\n\ndata: [DONE]\n\n"
	calls := 0
	source, err := newAgentContinuationSource(context.Background(), sseTestResponse(first), agentTestRequest(t), func(req *http.Request) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(req.Body)
		req.Body.Close()
		var payload struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Messages) != 3 || payload.Messages[1].Role != "assistant" || payload.Messages[1].Content != "文档说明支持 JSON 格式。换个格式：" || !strings.Contains(payload.Messages[2].Content, "next concrete action") {
			t.Fatalf("lost context: %s", body)
		}
		return sseTestResponse(second), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ra, recorder := newEmptyStreamTestAttempt(t, inbound.InboundTypeOpenAIResponse, model.APIFormatOpenAIResponse, outbound.OutboundTypeOpenAIChat)
	if err := ra.handleStreamResponseWithSourceV2(context.Background(), sseTestResponse(""), source); err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	for _, typ := range []string{"response.created", "response.completed"} {
		if strings.Count(body, `"type":"`+typ+`"`) != 1 {
			t.Fatalf("repeated or missing %s: %s", typ, body)
		}
	}
	if calls != 1 || strings.Contains(body, `"id":"followup"`) || !strings.Contains(body, "call_test") {
		t.Fatalf("bad continuation: calls=%d body=%s", calls, body)
	}
	var final struct {
		Response struct {
			ID    string `json:"id"`
			Usage struct {
				Input   int `json:"input_tokens"`
				Output  int `json:"output_tokens"`
				Total   int `json:"total_tokens"`
				Details struct {
					Cached int `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data:") && strings.Contains(line, `"type":"response.completed"`) {
			if err := json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &final); err != nil {
				t.Fatal(err)
			}
		}
	}
	if final.Response.ID != "initial" || final.Response.Usage.Input != 220 || final.Response.Usage.Output != 30 || final.Response.Usage.Total != 250 || final.Response.Usage.Details.Cached != 180 {
		t.Fatalf("incorrect combined response/usage: %+v", final)
	}
	if len(source.attempts()) != 1 {
		t.Fatal("continuation request not recorded")
	}
}

func TestAgentContinuationOnlyRetriesAnnouncementsAndHasLimit(t *testing.T) {
	for _, tc := range []struct {
		name, text, reason string
		wantCalls          int
		wantErr            bool
	}{
		{"complete", "All tests passed.", `"stop"`, 0, false},
		{"user question", "请提供 JSON 格式：", `"stop"`, 0, false},
		{"length", "现在补测试：", `"length"`, 0, false},
		{"truncated", "现在补测试：", `null`, 0, false},
		{"repeated announcement", "现在补测试：", `"stop"`, maxAgentContinuations, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			body := agentTestChunk("id", tc.text, tc.reason) + "data: [DONE]\n\n"
			s, err := newAgentContinuationSource(context.Background(), sseTestResponse(body), agentTestRequest(t), func(*http.Request) (*http.Response, error) { calls++; return sseTestResponse(body), nil })
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			for i := 0; i < 50; i++ {
				_, err = s.ReadEvent(context.Background())
				if err != nil {
					break
				}
			}
			if calls != tc.wantCalls || (err != nil && err != io.EOF) != tc.wantErr {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestAgentContinuationCancellationClosesPendingRequest(t *testing.T) {
	started := make(chan struct{})
	var canceled atomic.Bool
	body := agentTestChunk("id", "现在补测试：", `"stop"`) + "data: [DONE]\n\n"
	s, err := newAgentContinuationSource(context.Background(), sseTestResponse(body), agentTestRequest(t), func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		canceled.Store(true)
		return nil, req.Context().Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for {
			_, err := s.ReadEvent(context.Background())
			if err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("continuation did not start")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("continuation ignored cancellation")
	}
	if !canceled.Load() {
		t.Fatal("upstream request was not canceled")
	}
}
