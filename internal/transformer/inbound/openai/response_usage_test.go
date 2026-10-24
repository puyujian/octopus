package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bestruirui/octopus/internal/transformer/model"
	openaiOutbound "github.com/bestruirui/octopus/internal/transformer/outbound/openai"
)

func TestResponsesUsageCacheRoundTrip(t *testing.T) {
	for _, total := range []string{`,"total_tokens":3685`, "", `,"total_tokens":7141`} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("total=%s/stream=%t", total, streaming), func(t *testing.T) {
				ctx := context.Background()
				upstream := &openaiOutbound.ResponseOutbound{}
				downstream := &ResponseInbound{}
				body := fmt.Sprintf(`{"id":"resp_cache","object":"response","model":"cache-test","created_at":1710000000,"status":"completed","output":[{"type":"message","id":"msg_cache","role":"assistant","status":"completed","content":[{"type":"output_text","text":"OK","annotations":[]}]}],"usage":{"input_tokens":3677,"input_tokens_details":{"cached_tokens":3456},"output_tokens":8%s}}`, total)
				var wire []byte
				var err error
				if streaming {
					var events []model.StreamEvent
					events, err = upstream.TransformStreamEvent(ctx, []byte(`{"type":"response.completed","response":`+body+`}`))
					if err == nil {
						wire, err = downstream.TransformStreamEvents(ctx, events)
					}
				} else {
					var response *model.InternalLLMResponse
					response, err = upstream.TransformResponse(ctx, &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": {"application/json"}},
						Body:       io.NopCloser(strings.NewReader(body)),
					})
					if err == nil {
						wire, err = downstream.TransformResponse(ctx, response)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				var response ResponsesResponse
				if streaming {
					found := false
					for _, line := range strings.Split(string(wire), "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						var event ResponsesStreamEvent
						if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
							t.Fatal(err)
						}
						if event.Type == "response.completed" && event.Response != nil {
							response = *event.Response
							found = true
						}
					}
					if !found {
						t.Fatalf("missing terminal response: %s", wire)
					}
				} else if err := json.Unmarshal(wire, &response); err != nil {
					t.Fatal(err)
				}
				if u := response.Usage; u == nil || u.InputTokens != 3677 || u.OutputTokens != 8 || u.TotalTokens != 3685 || u.InputTokenDetails.CachedTokens != 3456 {
					t.Fatalf("cache must be an input subset and total must equal input + output: %+v", response.Usage)
				}
			})
		}
	}
}

func TestResponsesUsageAnthropicCache(t *testing.T) {
	usage := &model.Usage{
		PromptTokens:               221,
		CompletionTokens:           8,
		CacheReadInputTokens:       3456,
		CacheCreation5mInputTokens: 20,
		CacheCreation1hInputTokens: 10,
	}
	got := convertUsageToResponses(usage)
	if got.InputTokens != 3707 || got.TotalTokens != 3715 || got.InputTokenDetails.CachedTokens != 3456 {
		t.Fatalf("Anthropic's separate cache buckets must be counted once: %+v", got)
	}
}
