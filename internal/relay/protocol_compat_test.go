package relay

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	dbmodel "github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/bestruirui/octopus/internal/transformer/inbound"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/gin-gonic/gin"
)

const protocolTestResponse = `{"id":"resp_protocol","object":"response","created_at":1,"model":"gpt-5.6-luna","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}]}`

func TestRelayToolResultsWithSemanticOverride(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			ctx := setupRelayTestDB(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body, _ := io.ReadAll(r.Body)
				var payload struct {
					Input     []map[string]json.RawMessage `json:"input"`
					Reasoning struct {
						Effort string `json:"effort"`
					} `json:"reasoning"`
				}
				_ = json.Unmarshal(body, &payload)
				if payload.Reasoning.Effort != "max" {
					t.Errorf("semantic override not applied: %s", body)
				}
				if len(payload.Input) != 4 {
					t.Errorf("parallel round changed: %s", body)
				}
				for _, item := range payload.Input {
					if _, present := item["item_reference"]; present {
						w.WriteHeader(400)
						_, _ = io.WriteString(w, `{"error":{"message":"Unknown parameter: 'input[2].item_reference'."}}`)
						return
					}
				}
				for i, id := range []string{"call_a", "call_b", "call_a", "call_b"} {
					if i >= len(payload.Input) || rawJSONString(payload.Input[i]["call_id"]) != id {
						t.Errorf("call_id/order changed: %s", body)
					}
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":"+protocolTestResponse+"}\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, protocolTestResponse)
				}
			}))
			defer server.Close()
			channel := &dbmodel.Channel{Name: "strict-cpa", Type: outbound.OutboundTypeOpenAIResponse, Enabled: true, BaseUrls: []dbmodel.BaseUrl{{URL: server.URL + "/v1"}}, Model: "gpt-5.6-luna", Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}}}
			if err := op.ChannelCreate(channel, ctx); err != nil {
				t.Fatal(err)
			}
			override := `{"$octopus":{"reasoning_effort":"max"}}`
			group := &dbmodel.Group{Name: "strict-protocol", Mode: dbmodel.GroupModeFailover, ParamOverride: &override, FirstTokenTimeOut: 5}
			if err := op.GroupCreate(group, ctx); err != nil {
				t.Fatal(err)
			}
			if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: channel.ID, ModelName: "gpt-5.6-luna", Priority: 1, Weight: 1}, ctx); err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"model":"strict-protocol","stream":%t,"input":[{"type":"function_call","call_id":"call_a","name":"f","arguments":"{}"},{"type":"function_call","call_id":"call_b","name":"f","arguments":"{}"},{"type":"function_call_output","call_id":"call_a","output":"a"},{"type":"function_call_output","call_id":"call_b","output":"b"}]}`, stream)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			Handler(inbound.InboundTypeOpenAIResponse, c)
			if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "OK") || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d body=%s", recorder.Code, calls.Load(), recorder.Body.String())
			}
		})
	}
}

func TestRelayHTMLMaintenancePageFallsBackBeforeWriting(t *testing.T) {
	for _, contentType := range []string{"text/html", "application/json"} {
		t.Run(contentType, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			ctx := setupRelayTestDB(t)
			var primaryCalls, backupCalls atomic.Int32
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls.Add(1)
				w.Header().Set("Content-Type", contentType)
				_, _ = io.WriteString(w, "<!DOCTYPE html><html>Maintenance</html>")
			}))
			defer primary.Close()
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backupCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, protocolTestResponse)
			}))
			defer backup.Close()
			group := &dbmodel.Group{Name: "html-protocol", Mode: dbmodel.GroupModeFailover, FirstTokenTimeOut: 5}
			if err := op.GroupCreate(group, ctx); err != nil {
				t.Fatal(err)
			}
			for i, server := range []*httptest.Server{primary, backup} {
				channel := &dbmodel.Channel{Name: fmt.Sprintf("html-%d", i), Type: outbound.OutboundTypeOpenAIResponse, Enabled: true, BaseUrls: []dbmodel.BaseUrl{{URL: server.URL + "/v1"}}, Model: "gpt-5.6-luna", Keys: []dbmodel.ChannelKey{{Enabled: true, ChannelKey: "test-key"}}}
				if err := op.ChannelCreate(channel, ctx); err != nil {
					t.Fatal(err)
				}
				if err := op.GroupItemAdd(&dbmodel.GroupItem{GroupID: group.ID, ChannelID: channel.ID, ModelName: "gpt-5.6-luna", Priority: i + 1, Weight: 1}, ctx); err != nil {
					t.Fatal(err)
				}
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"html-protocol","input":"hello"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			Handler(inbound.InboundTypeOpenAIResponse, c)
			if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "OK") || strings.Contains(recorder.Body.String(), "Maintenance") || primaryCalls.Load() != 1 || backupCalls.Load() != 1 {
				t.Fatalf("status=%d primary=%d backup=%d body=%s", recorder.Code, primaryCalls.Load(), backupCalls.Load(), recorder.Body.String())
			}
		})
	}
}
