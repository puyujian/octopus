package relay

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/bestruirui/octopus/internal/transformer/outbound"
	"github.com/bestruirui/octopus/internal/utils/log"
)

type promptCacheKeyCompatibilityKey struct {
	channelID int
	model     string
	endpoint  string
}

// Scope learned rejections to the actual endpoint as well as the channel/model,
// so a different protocol or a changed base URL keeps its cache support.
var promptCacheKeyCompatibility sync.Map

// sendRequest handles the optional cache hint after all transformations and
// overrides. Both normalized and raw passthrough requests use this path.
func (ra *relayAttempt) sendRequest(req *http.Request) (*http.Response, error) {
	if ra.channel.Type != outbound.OutboundTypeOpenAIChat &&
		ra.channel.Type != outbound.OutboundTypeOpenAIResponse {
		return ra.sendRequestOnce(req)
	}

	// Keep a replayable body even for adapters that do not provide GetBody.
	if req.Body != nil && req.GetBody == nil {
		body, err := readOutboundRequestBody(req)
		if err != nil {
			return nil, err
		}
		setPromptCacheCompatibilityBody(req, body)
	}
	key := promptCacheKeyCompatibilityKey{
		channelID: ra.channel.ID,
		model:     ra.internalRequest.Model,
		endpoint:  req.URL.Scheme + "://" + req.URL.Host + req.URL.EscapedPath(),
	}
	if _, unsupported := promptCacheKeyCompatibility.Load(key); unsupported {
		if _, err := ra.omitUnsupportedPromptCacheKey(req); err != nil {
			return nil, err
		}
	}

	response, err := ra.sendRequestOnce(req)
	if err != nil || response.StatusCode != http.StatusBadRequest {
		return response, err
	}
	// Restore non-matching errors for the existing semantic/developer-role
	// compatibility handlers, including their original status and headers.
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("failed to read rejected response body: %w", readErr)
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	if unsupportedUpstreamParameter(body) != "prompt_cache_key" {
		return response, nil
	}
	changed, err := ra.omitUnsupportedPromptCacheKey(req)
	if err != nil {
		return nil, err
	}
	if !changed {
		return response, nil
	}

	promptCacheKeyCompatibility.Store(key, struct{}{})
	ra.retryAfter = 0
	log.Warnf("channel %s model %s rejected prompt_cache_key; retrying once without it", ra.channel.Name, ra.internalRequest.Model)
	return ra.sendRequestOnce(req)
}

func (ra *relayAttempt) omitUnsupportedPromptCacheKey(req *http.Request) (bool, error) {
	body, err := readOutboundRequestBody(req)
	if err != nil {
		return false, err
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return false, nil
	}
	if _, present := payload["prompt_cache_key"]; !present {
		return false, nil
	}
	delete(payload, "prompt_cache_key")
	modified, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	setPromptCacheCompatibilityBody(req, modified)
	ra.metrics.SetTransportRequestPayload(modified, ra.internalRequest.Model)
	return true, nil
}

func setPromptCacheCompatibilityBody(req *http.Request, body []byte) {
	if req.Body != nil {
		_ = req.Body.Close()
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	req.Header.Del("Content-Length")
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}
