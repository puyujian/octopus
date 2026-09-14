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
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/transformer/outbound"
)

const searchPreviewRejection = `{"error":{"message":"Input should be 'function', 'web_search_preview' or 'code_interpreter', field: 'tools[1].type', value: 'web_search'","code":"invalid_request_error"}}`
const searchNestedRejection = `{"error":{"message":"tools[1].web_search can not be null","code":"1214"}}`
const searchFunctionRejection = "{\"error\":{\"message\":\"The request failed because it is missing `***.function` parameter. Request id: test\",\"code\":\"MissingParameter\"}}"
const searchCompatibilityRequest = `{"model":"glm-5.3","stream":true,"input":[{"role":"user","content":"hello"}],"tools":[{"type":"function","name":"noop","parameters":{"type":"object","properties":{}}},{"type":"web_search"}],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"noop"},{"type":"web_search"}]},"provider_extension":{"keep":true}}`

func TestSendRequestSearchSchemaCompatibility(t *testing.T) {
	setupRelayTestDB(t)
	for _, tc := range []struct {
		name, rejection, wantType string
		nested                    bool
	}{
		{"preview", searchPreviewRejection, "web_search_preview", false},
		{"missing legacy function", searchFunctionRejection, "web_search_preview", false},
		{"nested search", searchNestedRejection, "web_search", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetPromptCacheKeyCompatibility(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var payload map[string]json.RawMessage
				_ = json.Unmarshal(body, &payload)
				var tools []map[string]json.RawMessage
				_ = json.Unmarshal(payload["tools"], &tools)
				if r.ContentLength != int64(len(body)) {
					t.Errorf("Content-Length mismatch")
				}
				if calls.Add(1) == 1 {
					if string(body) != searchCompatibilityRequest {
						t.Errorf("initial request changed: %s", body)
					}
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, tc.rejection)
					return
				}
				if len(tools) != 2 || rawJSONString(tools[0]["name"]) != "noop" || rawJSONString(tools[1]["type"]) != tc.wantType {
					t.Errorf("tools lost/reordered: %s", body)
				}
				if tc.nested && string(tools[1]["web_search"]) != `{"enable":true}` {
					t.Errorf("search not enabled: %s", body)
				}
				if string(payload["model"]) != `"glm-5.3"` || string(payload["stream"]) != "true" || string(payload["provider_extension"]) != `{"keep":true}` {
					t.Errorf("unrelated fields changed: %s", body)
				}
				var choice struct {
					Tools []struct {
						Type string `json:"type"`
					} `json:"tools"`
				}
				_ = json.Unmarshal(payload["tool_choice"], &choice)
				if len(choice.Tools) != 2 || choice.Tools[1].Type != tc.wantType {
					t.Errorf("tool choice did not follow search version: %s", body)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: ok\n\n")
			}))
			defer server.Close()
			ra := newSearchCompatibilityAttempt()
			req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(searchCompatibilityRequest))
			req.GetBody = nil
			response, err := ra.sendRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != 200 || string(body) != "data: ok\n\n" || calls.Load() != 2 {
				t.Fatalf("status=%d calls=%d body=%s", response.StatusCode, calls.Load(), body)
			}
		})
	}
}

func newSearchCompatibilityAttempt() *relayAttempt {
	internal := &transformerModel.InternalLLMRequest{Model: "glm-5.3"}
	return &relayAttempt{
		relayRequest: &relayRequest{internalRequest: internal, metrics: NewRelayMetrics(1, internal.Model, nil, internal)},
		channel:      &dbmodel.Channel{ID: 1, Name: "search-compat", Type: outbound.OutboundTypeOpenAIResponse},
	}
}

func TestSearchCompatibilityPreservesConstraintsAndUnrelatedErrors(t *testing.T) {
	for _, tc := range []struct{ name, request, rejection string }{
		{"unrelated validation", searchCompatibilityRequest, `{"error":{"message":"Invalid function arguments"}}`},
		{"wrong tool index", searchCompatibilityRequest, strings.ReplaceAll(searchPreviewRejection, "tools[1]", "tools[0]")},
		{"out of range", searchCompatibilityRequest, strings.ReplaceAll(searchPreviewRejection, "tools[1]", "tools[97]")},
		{"non JSON error", searchCompatibilityRequest, "<html>web_search_preview</html>"},
		{"other native tool missing function", strings.Replace(searchCompatibilityRequest, `"type":"function","name":"noop"`, `"type":"code_interpreter","name":"noop"`, 1), searchFunctionRejection},
		{"domain allowlist", strings.Replace(searchCompatibilityRequest, `{"type":"web_search"}`, `{"type":"web_search","filters":{"allowed_domains":["example.org"]}}`, 1), searchPreviewRejection},
		{"offline search", strings.Replace(searchCompatibilityRequest, `{"type":"web_search"}`, `{"type":"web_search","external_web_access":false}`, 1), searchPreviewRejection},
		{"explicit disabled nested search", strings.Replace(searchCompatibilityRequest, `{"type":"web_search"}`, `{"type":"web_search","web_search":{"enable":false}}`, 1), searchPreviewRejection},
		{"nested does not drop location", strings.Replace(searchCompatibilityRequest, `{"type":"web_search"}`, `{"type":"web_search","user_location":{"country":"CN"}}`, 1), searchNestedRejection},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if body, format := adaptRejectedWebSearch([]byte(tc.request), []byte(tc.rejection)); len(body) > 0 {
				t.Fatalf("unexpected %s adaptation: %s", format, body)
			}
		})
	}
}

func TestSearchCompatibilityRetriesAreBoundedAndPreserveErrors(t *testing.T) {
	setupRelayTestDB(t)
	for _, tc := range []struct {
		name              string
		status, wantCalls int
		alternate         bool
	}{
		{"authentication", 401, 1, false}, {"rate limit", 429, 1, false}, {"unavailable", 503, 1, false},
		{"same rejection", 400, 2, false}, {"alternating providers", 400, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetPromptCacheKeyCompatibility(t)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(tc.status)
				if tc.alternate && n == 2 {
					_, _ = io.WriteString(w, searchNestedRejection)
				} else {
					_, _ = io.WriteString(w, searchPreviewRejection)
				}
			}))
			defer server.Close()
			req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(searchCompatibilityRequest))
			response, err := newSearchCompatibilityAttempt().sendRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != tc.status || string(body) != searchPreviewRejection || response.Header.Get("Retry-After") != "7" || int(calls.Load()) != tc.wantCalls {
				t.Fatalf("status=%d calls=%d body=%s", response.StatusCode, calls.Load(), body)
			}
		})
	}
}

func TestSearchCompatibilityCoexistsWithPromptCacheRetry(t *testing.T) {
	setupRelayTestDB(t)
	resetPromptCacheKeyCompatibility(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n := calls.Add(1)
		if n > 1 && strings.Contains(string(body), "prompt_cache_key") {
			t.Error("rejected cache hint reintroduced")
		}
		if n < 3 {
			w.WriteHeader(400)
			if n == 1 {
				_, _ = io.WriteString(w, promptCacheKeyRejection)
			} else {
				_, _ = io.WriteString(w, searchPreviewRejection)
			}
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	input := strings.Replace(searchCompatibilityRequest, `"model":`, `"prompt_cache_key":"test","model":`, 1)
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(input))
	response, err := newSearchCompatibilityAttempt().sendRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || calls.Load() != 3 {
		t.Fatal(fmt.Sprintf("status=%d calls=%d", response.StatusCode, calls.Load()))
	}
}
