package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/bestruirui/octopus/internal/transformer/bridge"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

// AxonStreamBridge adapts Octopus' one-chunk-at-a-time relay callback to
// AxonHub's pull-based streaming transformer.  The response queue is an
// unbounded, mutex-protected slice rather than a bounded channel: a single
// Responses event can produce multiple unified responses, and a bounded
// channel can deadlock the feeder before it can announce the event boundary.
type AxonStreamBridge struct {
	inner  transformer.Outbound
	feeder *bridge.ChunkFeeder
	// request is the concrete AxonHub request produced while building the
	// upstream HTTP request.  Responses/stream transformers use it to route
	// request-type-specific events and to retain transformer metadata.
	request *httpclient.Request

	started   sync.Once
	startMu   sync.RWMutex
	stream    streams.Stream[*llm.Response]
	startErr  error
	streamErr error
	done      chan struct{}

	queueMu sync.Mutex
	queue   []*llm.Response

	feedMu    sync.Mutex
	closeOnce sync.Once
}

// NewAxonStreamBridge creates a chunk-driven bridge over an AxonHub stream
// transformer.
func NewAxonStreamBridge(inner transformer.Outbound) *AxonStreamBridge {
	return &AxonStreamBridge{
		inner:  inner,
		feeder: bridge.NewChunkFeeder(),
		done:   make(chan struct{}),
	}
}

// SetRequest attaches the request that belongs to the response stream.  It is
// intentionally a setter rather than a constructor argument because outbound
// adapters are created before TransformRequest is called, while the AxonHub
// request only exists after that conversion has completed.
func (b *AxonStreamBridge) SetRequest(request *httpclient.Request) {
	if b == nil {
		return
	}
	b.startMu.Lock()
	b.request = request
	b.startMu.Unlock()
}

func (b *AxonStreamBridge) start(ctx context.Context) {
	if b == nil || b.inner == nil {
		if b != nil {
			b.startMu.Lock()
			b.startErr = fmt.Errorf("AxonHub stream transformer is nil")
			b.startMu.Unlock()
			close(b.done)
		}
		return
	}
	b.startMu.RLock()
	request := b.request
	b.startMu.RUnlock()
	stream, err := b.inner.TransformStream(ctx, request, b.feeder)
	if err != nil {
		b.startMu.Lock()
		b.startErr = fmt.Errorf("failed to init AxonHub stream transformer: %w", err)
		b.startMu.Unlock()
		close(b.done)
		return
	}
	if stream == nil {
		b.startMu.Lock()
		b.startErr = fmt.Errorf("AxonHub stream transformer returned nil stream")
		b.startMu.Unlock()
		close(b.done)
		return
	}
	b.startMu.Lock()
	b.stream = stream
	b.startMu.Unlock()

	go func() {
		defer func() {
			streamErr := stream.Err()
			b.startMu.Lock()
			b.streamErr = streamErr
			b.startMu.Unlock()
			_ = stream.Close()
			close(b.done)
		}()
		for stream.Next() {
			response := stream.Current()
			if response == nil {
				continue
			}
			b.queueMu.Lock()
			b.queue = append(b.queue, response)
			b.queueMu.Unlock()
		}
	}()
}

func (b *AxonStreamBridge) streamState() (streams.Stream[*llm.Response], error) {
	if b == nil {
		return nil, fmt.Errorf("nil AxonHub stream bridge")
	}
	b.startMu.RLock()
	stream, err := b.stream, b.startErr
	b.startMu.RUnlock()
	return stream, err
}

func (b *AxonStreamBridge) streamError() error {
	if b == nil {
		return nil
	}
	b.startMu.RLock()
	defer b.startMu.RUnlock()
	if b.streamErr != nil {
		return b.streamErr
	}
	return b.startErr
}

func (b *AxonStreamBridge) streamErrorResponse() *InternalLLMResponse {
	streamErr := b.streamError()
	if streamErr == nil {
		return nil
	}
	var responseErr *llm.ResponseError
	if !errors.As(streamErr, &responseErr) || responseErr == nil {
		return nil
	}
	return &InternalLLMResponse{Error: FromLLMResponseError(responseErr)}
}

// Feed transforms one provider stream payload.  A nil response means that
// the provider event is valid but has no client-visible unified chunk (ping,
// response.created bookkeeping, etc.).
func (b *AxonStreamBridge) Feed(ctx context.Context, data []byte) (*InternalLLMResponse, error) {
	if b == nil {
		return nil, fmt.Errorf("nil AxonHub stream bridge")
	}
	if len(data) == 0 {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	b.feedMu.Lock()
	defer b.feedMu.Unlock()
	b.started.Do(func() { b.start(ctx) })
	if _, err := b.streamState(); err != nil {
		return nil, err
	}
	b.drainResponses()
	// Responses/Anthropic transformers emit their semantic terminal response
	// and then finish their pull stream when the provider's terminal event is
	// observed. The relay can still deliver the transport-level [DONE] marker
	// afterwards. It is idempotent at this point: return the shared terminal
	// response instead of feeding a closed ChunkFeeder a second time.
	if isDoneMarker(data) {
		select {
		case <-b.done:
			if streamErr := b.streamError(); streamErr != nil {
				if response := b.streamErrorResponse(); response != nil {
					return response, nil
				}
				return nil, streamErr
			}
			return &InternalLLMResponse{Object: "[DONE]", APIFormat: APIFormat(b.inner.APIFormat())}, nil
		default:
		}
	}

	eventType := streamEventType(data)
	seq, err := b.feeder.Feed(ctx, data, eventType)
	if err != nil {
		if errors.Is(err, io.ErrClosedPipe) {
			return nil, fmt.Errorf("AxonHub stream is closed")
		}
		return nil, err
	}
	if err := waitForSequence(ctx, b.done, b.feeder.Ack(), seq, "acknowledge"); err != nil {
		b.Close()
		if response := b.streamErrorResponse(); response != nil {
			return response, nil
		}
		return nil, err
	}
	if err := waitForSequence(ctx, b.done, b.feeder.Idle(), seq, "finish"); err != nil {
		b.Close()
		if response := b.streamErrorResponse(); response != nil {
			return response, nil
		}
		return nil, err
	}

	responses := b.drainResponses()

	// [DONE] is a transport marker for the relay, but AxonHub Responses and
	// Anthropic transformers need EOF to emit their unified terminal marker.
	// The provider terminal event has the same boundary semantics: after it has
	// crossed the feeder, close the source and collect the finish/usage/Done
	// responses emitted while the AxonHub transformer observes EOF. Without
	// this, a Responses stream which ends at response.completed leaves the
	// transformer blocked in Next forever and a Gemini finish event can lose its
	// appended Done response.
	if b.shouldCloseAfterEvent(data, eventType, responses) {
		b.Close()
		select {
		case <-b.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if streamErr := b.streamError(); streamErr != nil {
			// In particular, do not turn a bare [DONE] into a successful
			// response for OpenAI Responses: its transformer deliberately
			// reports ErrStreamIncomplete when no semantic terminal event was
			// observed, so the relay can retry the attempt safely.
			if errors.Is(streamErr, llm.ErrStreamIncomplete) {
				return nil, streamErr
			}
			if response := b.streamErrorResponse(); response != nil {
				return response, nil
			}
			return nil, streamErr
		}
		responses = append(responses, b.drainResponses()...)
	}

	result := mergeAxonChunks(responses)
	if result != nil {
		if result.APIFormat == "" {
			result.APIFormat = APIFormat(b.inner.APIFormat())
		}
		if result.RequestType == "" {
			result.RequestType = "chat"
		}
		NormalizeResponseUsageForAPI(result)
		if result.APIFormat == APIFormatAnthropicMessage {
			RestoreAnthropicSignatures(result)
		}
		attachRawResponsesOutput(result, data)
	}
	return result, nil
}

func isDoneMarker(data []byte) bool {
	return strings.HasPrefix(strings.TrimSpace(string(data)), "[DONE]")
}

func (b *AxonStreamBridge) shouldCloseAfterEvent(data []byte, eventType string, responses []*llm.Response) bool {
	if isDoneMarker(data) {
		return true
	}

	switch eventType {
	case "response.completed", "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "message_stop":
		return true
	case "error":
		// Provider error events are terminal for the local stream even when
		// the transformer exposes the error as a response rather than a Go
		// error.
		return true
	}

	// Gemini's native stream has no SSE event type. Its finishReason is
	// carried inside the candidate response and the AxonHub Gemini
	// transformer includes it in the unified response choice.
	if b != nil && b.inner != nil && b.inner.APIFormat() == llm.APIFormatGeminiContents {
		for _, response := range responses {
			if response == nil {
				continue
			}
			for _, choice := range response.Choices {
				if choice.FinishReason != nil {
					return true
				}
			}
		}
	}
	return false
}

func waitForSequence[T ~uint64](ctx context.Context, done <-chan struct{}, events <-chan T, wanted uint64, phase string) error {
	for {
		select {
		case value := <-events:
			if uint64(value) >= wanted {
				return nil
			}
		case <-done:
			return fmt.Errorf("AxonHub stream stopped before %s event", phase)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func streamEventType(data []byte) string {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &probe) == nil {
		return strings.TrimSpace(probe.Type)
	}
	return ""
}

func (b *AxonStreamBridge) drainResponses() []*llm.Response {
	if b == nil {
		return nil
	}
	b.queueMu.Lock()
	defer b.queueMu.Unlock()
	if len(b.queue) == 0 {
		return nil
	}
	responses := b.queue
	b.queue = nil
	return responses
}

func (b *AxonStreamBridge) Close() {
	if b == nil {
		return
	}
	b.closeOnce.Do(func() {
		_ = b.feeder.Close()
	})
}

// mergeAxonChunks folds all responses produced while processing one provider
// event.  Responses emits a content/finish response followed by a usage-only
// response for some terminal events.
func mergeAxonChunks(chunks []*llm.Response) *InternalLLMResponse {
	var out *InternalLLMResponse
	for _, chunk := range chunks {
		if chunk == nil {
			continue
		}
		converted := FromLLMResponse(chunk)
		if converted == nil {
			continue
		}
		if out == nil {
			out = converted
			continue
		}
		mergeInternalResponse(out, converted)
	}
	return out
}

func mergeInternalResponse(dst, src *InternalLLMResponse) {
	if dst == nil || src == nil {
		return
	}
	if dst.ID == "" {
		dst.ID = src.ID
	}
	if dst.Model == "" {
		dst.Model = src.Model
	}
	if dst.Object == "" {
		dst.Object = src.Object
	}
	if dst.Created == 0 {
		dst.Created = src.Created
	}
	if dst.PreviousResponseID == nil {
		dst.PreviousResponseID = cloneStringPtr(src.PreviousResponseID)
	}
	// A single provider event can produce a finish chunk followed by a
	// usage-only chunk. The latter is authoritative even when an earlier chunk
	// already carried provisional usage (for example Responses response.created).
	if src.Usage != nil {
		dst.Usage = src.Usage
	}
	if dst.Error == nil && src.Error != nil {
		dst.Error = src.Error
	}
	if len(src.Choices) > 0 {
		dst.Choices = append(dst.Choices, src.Choices...)
	}
	if len(src.EmbeddingData) > 0 {
		dst.EmbeddingData = append(dst.EmbeddingData, src.EmbeddingData...)
	}
	if len(dst.TransformerMetadata) == 0 && len(src.TransformerMetadata) > 0 {
		dst.TransformerMetadata = cloneAnyMap(src.TransformerMetadata)
	}
	if dst.RequestType == "" {
		dst.RequestType = src.RequestType
	}
	if dst.APIFormat == "" {
		dst.APIFormat = src.APIFormat
	}
}

func attachRawResponsesOutput(response *InternalLLMResponse, data []byte) {
	if response == nil || response.APIFormat != APIFormatOpenAIResponse {
		// Stream chunks from Responses have no APIFormat in older AxonHub
		// releases; identify them by the event shape as a compatibility path.
		if response == nil || !strings.Contains(string(data), "response.completed") {
			return
		}
	}
	var event struct {
		Response struct {
			Output json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &event) == nil && len(event.Response.Output) > 0 {
		response.RawResponsesOutputItems = cloneRawMessage(event.Response.Output)
	}
}

// TransformResponse drives AxonHub's non-streaming response converter. Error
// bodies are normalized before calling AxonHub because several provider
// transformers intentionally return a generic HTTP error for status >= 400.
func TransformResponse(ctx context.Context, inner transformer.Outbound, response *http.Response) (*InternalLLMResponse, error) {
	if response == nil {
		return nil, fmt.Errorf("response is nil")
	}
	if response.Body == nil {
		return nil, fmt.Errorf("response body is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("response body is empty")
	}
	if response.StatusCode >= http.StatusBadRequest {
		if responseErr := parseProviderResponseError(response.StatusCode, response.Header, body); responseErr != nil {
			return nil, responseErr
		}
		return nil, fmt.Errorf("HTTP error %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	if inner == nil {
		return nil, fmt.Errorf("AxonHub response transformer is nil")
	}
	llmResponse, err := inner.TransformResponse(ctx, &httpclient.Response{
		StatusCode: response.StatusCode,
		Headers:    response.Header,
		Body:       body,
		Request:    requestMetadata(response),
	})
	if err != nil {
		return nil, err
	}
	converted := FromLLMResponse(llmResponse)
	if converted != nil {
		if converted.APIFormat == "" {
			converted.APIFormat = APIFormat(inner.APIFormat())
		}
		NormalizeResponseUsageForAPI(converted)
		if converted.RequestType == "" {
			converted.RequestType = string(llmResponse.RequestType)
			if converted.RequestType == "" {
				converted.RequestType = "chat"
			}
		}
		attachRawNonStreamingOutput(converted, body)
	}
	return converted, nil
}

// BuildAxonHTTPRequest runs the unified request through AxonHub's outbound
// converter and adapts the resulting structured request to net/http.  Keeping
// this in one place makes all provider adapters carry the same API format,
// request type, metadata, raw request, authentication and query semantics.
func BuildAxonHTTPRequest(ctx context.Context, inner transformer.Outbound, request *InternalLLMRequest) (*http.Request, *httpclient.Request, error) {
	if inner == nil {
		return nil, nil, fmt.Errorf("AxonHub request transformer is nil")
	}
	if request == nil {
		return nil, nil, fmt.Errorf("request is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	llmRequest, err := ToLLMRequest(request)
	if err != nil {
		return nil, nil, err
	}
	// AxonHub's Responses and Gemini transformers inspect RawRequest for
	// protocol-specific routing, headers and body-preserving behavior. Provide
	// the complete runtime carrier that was captured at ingress.
	llmRequest.RawRequest = &httpclient.Request{
		Method:      http.MethodPost,
		Path:        request.RawPath,
		Body:        append([]byte(nil), request.RawRequest...),
		Query:       cloneURLValues(request.Query),
		Headers:     request.RawHeaders.Clone(),
		RawRequest:  rawHTTPRequestForAxon(request),
		APIFormat:   string(request.RawAPIFormat),
		RequestType: string(requestTypeFor(request)),
	}
	if len(request.RawRequest) > 0 {
		llmRequest.RawRequest.JSONBody = append([]byte(nil), request.RawRequest...)
	}

	axonRequest, err := inner.TransformRequest(ctx, llmRequest)
	if err != nil {
		return nil, nil, err
	}
	if axonRequest == nil {
		return nil, nil, fmt.Errorf("AxonHub request transformer returned nil request")
	}
	if axonRequest.APIFormat == "" {
		axonRequest.APIFormat = string(inner.APIFormat())
	}
	if axonRequest.RequestType == "" {
		axonRequest.RequestType = string(requestTypeFor(request))
	}
	// Query belongs to the inbound request, while AxonHub may also put
	// provider-required values (for example Gemini alt=sse) in URL. Preserve
	// the generated query map and add inbound values only when the provider
	// explicitly allows inbound query merging. The HTTP builder then combines
	// this map with the query already present in axonRequest.URL.
	if !axonRequest.SkipInboundQueryMerge {
		generatedQuery := cloneURLValues(axonRequest.Query)
		if parsed, parseErr := url.Parse(axonRequest.URL); parseErr == nil {
			generatedQuery = mergeURLValues(parsed.Query(), generatedQuery)
		}
		axonRequest.Query = mergeURLValues(generatedQuery, request.Query)
	}

	httpRequest, err := bridge.BuildHTTPRequest(ctx, axonRequest)
	if err != nil {
		return nil, nil, err
	}
	return httpRequest, axonRequest, nil
}

func rawHTTPRequestForAxon(request *InternalLLMRequest) *http.Request {
	if request == nil {
		return nil
	}
	raw := request.RawHTTPRequest
	if raw == nil && (request.RawPath != "" || request.RawHeaders != nil) {
		raw = &http.Request{
			Method: http.MethodPost,
			Header: request.RawHeaders.Clone(),
			URL:    &url.URL{Path: request.RawPath},
		}
	}
	if raw == nil || raw.PathValue("gemini-api-version") != "" {
		return raw
	}
	version := firstGeminiVersionSegment(request.RawPath)
	if version == "" {
		return raw
	}
	clone := raw.Clone(raw.Context())
	clone.SetPathValue("gemini-api-version", version)
	return clone
}

func firstGeminiVersionSegment(path string) string {
	for _, segment := range strings.Split(strings.Trim(path, "/"), "/") {
		if len(segment) >= 2 && segment[0] == 'v' && segment[1] >= '0' && segment[1] <= '9' {
			return segment
		}
	}
	return ""
}

func mergeURLValues(generated, inbound url.Values) url.Values {
	merged := cloneURLValues(generated)
	if len(inbound) == 0 {
		return merged
	}
	if merged == nil {
		merged = make(url.Values)
	}
	for key, values := range inbound {
		if _, exists := merged[key]; exists {
			continue
		}
		merged[key] = append([]string(nil), values...)
	}
	return merged
}

// MergeJSONFields copies selected top-level fields from overlay into base.
// It is used only for provider fields that are intentionally outside AxonHub's
// common request model; the provider's message/tool conversion remains owned
// by AxonHub.
func MergeJSONFields(base, overlay []byte, fields ...string) ([]byte, error) {
	if len(base) == 0 {
		return append([]byte(nil), overlay...), nil
	}
	var baseObject map[string]json.RawMessage
	if err := json.Unmarshal(base, &baseObject); err != nil {
		return nil, fmt.Errorf("decode base request JSON: %w", err)
	}
	var overlayObject map[string]json.RawMessage
	if err := json.Unmarshal(overlay, &overlayObject); err != nil {
		return nil, fmt.Errorf("decode overlay request JSON: %w", err)
	}
	for _, field := range fields {
		if value, ok := overlayObject[field]; ok {
			baseObject[field] = append(json.RawMessage(nil), value...)
		}
	}
	return json.Marshal(baseObject)
}

// MergeJSONObjects merges selected nested JSON objects while retaining fields
// generated by AxonHub that are absent from the provider-specific overlay.
func MergeJSONObjects(base, overlay []byte, fields ...string) ([]byte, error) {
	if len(base) == 0 {
		return append([]byte(nil), overlay...), nil
	}
	var baseObject map[string]json.RawMessage
	if err := json.Unmarshal(base, &baseObject); err != nil {
		return nil, fmt.Errorf("decode base request JSON: %w", err)
	}
	var overlayObject map[string]json.RawMessage
	if err := json.Unmarshal(overlay, &overlayObject); err != nil {
		return nil, fmt.Errorf("decode overlay request JSON: %w", err)
	}
	for _, field := range fields {
		overlayValue, ok := overlayObject[field]
		if !ok {
			continue
		}
		var overlayMap map[string]json.RawMessage
		if json.Unmarshal(overlayValue, &overlayMap) != nil {
			baseObject[field] = append(json.RawMessage(nil), overlayValue...)
			continue
		}
		merged := map[string]json.RawMessage{}
		if baseValue, ok := baseObject[field]; ok {
			var baseMap map[string]json.RawMessage
			if json.Unmarshal(baseValue, &baseMap) == nil {
				for key, value := range baseMap {
					merged[key] = value
				}
			}
		}
		for key, value := range overlayMap {
			merged[key] = value
		}
		encoded, err := json.Marshal(merged)
		if err != nil {
			return nil, fmt.Errorf("encode merged %s: %w", field, err)
		}
		baseObject[field] = encoded
	}
	return json.Marshal(baseObject)
}

func cloneURLValues(values url.Values) url.Values {
	if values == nil {
		return nil
	}
	cloned := make(url.Values, len(values))
	for key, items := range values {
		cloned[key] = append([]string(nil), items...)
	}
	return cloned
}

func requestMetadata(response *http.Response) *httpclient.Request {
	if response == nil || response.Request == nil {
		return nil
	}
	if metadata := bridge.RequestFromHTTPContext(response.Request); metadata != nil {
		return metadata
	}
	return &httpclient.Request{RawRequest: response.Request, Headers: response.Request.Header.Clone()}
}

func attachRawNonStreamingOutput(response *InternalLLMResponse, body []byte) {
	if response == nil || response.APIFormat != APIFormatOpenAIResponse {
		return
	}
	var payload struct {
		Output json.RawMessage `json:"output"`
	}
	if json.Unmarshal(body, &payload) == nil && len(payload.Output) > 0 {
		response.RawResponsesOutputItems = cloneRawMessage(payload.Output)
	}
}

func parseProviderResponseError(status int, headers http.Header, body []byte) *ResponseError {
	var envelope struct {
		Error struct {
			Code      json.RawMessage `json:"code"`
			Message   string          `json:"message"`
			Type      string          `json:"type"`
			Status    string          `json:"status"`
			Param     string          `json:"param"`
			RequestID string          `json:"request_id"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Error.Message == "" {
		return nil
	}
	code := ""
	if len(envelope.Error.Code) > 0 {
		var textCode string
		if json.Unmarshal(envelope.Error.Code, &textCode) == nil {
			code = textCode
		} else {
			code = strings.Trim(string(envelope.Error.Code), `"`)
		}
	}
	requestID := envelope.Error.RequestID
	if requestID == "" && headers != nil {
		requestID = headers.Get("x-request-id")
	}
	typeName := envelope.Error.Type
	if typeName == "" {
		typeName = envelope.Error.Status
	}
	return &ResponseError{StatusCode: status, Detail: ErrorDetail{
		Code: code, Message: envelope.Error.Message, Type: typeName,
		Param: envelope.Error.Param, RequestID: requestID,
	}}
}
