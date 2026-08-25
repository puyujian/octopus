package helper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

var protectedGroupOverrideKeys = map[string]struct{}{
	"model":  {},
	"stream": {},
	"type":   {},
}

// ValidateJSONOverrideObject validates the JSON shape used by parameter
// overrides. Empty values mean "clear the override" and are valid.
func ValidateJSONOverrideObject(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return err
	}
	if decoded == nil {
		return fmt.Errorf("param_override must be a JSON object")
	}
	return nil
}

// ValidateGroupParamOverride validates a group override and rejects fields
// owned by routing/transport layers.
func ValidateGroupParamOverride(value string) error {
	if err := ValidateJSONOverrideObject(value); err != nil {
		return err
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return err
	}
	for key := range decoded {
		if _, protected := protectedGroupOverrideKeys[strings.ToLower(strings.TrimSpace(key))]; protected {
			return fmt.Errorf("param_override cannot override protected field %q", key)
		}
	}
	return nil
}

// ApplyParamOverride merges a JSON-object override into an outbound JSON
// request body using the historical channel-level shallow merge semantics.
// Empty overrides, nil bodies, and non-object request bodies are ignored.
func ApplyParamOverride(request *http.Request, paramOverride *string) error {
	if request == nil || request.Body == nil || paramOverride == nil || strings.TrimSpace(*paramOverride) == "" {
		return nil
	}

	body, err := io.ReadAll(request.Body)
	if err != nil {
		return fmt.Errorf("failed to read request body: %w", err)
	}

	modifiedBody, changed, err := ApplyJSONParamOverride(body, paramOverride, false, false)
	if err != nil {
		// Preserve the historical behavior for malformed channel values: the
		// original request is sent unchanged and the relay remains available.
		setRequestBody(request, body)
		return nil
	}
	if !changed {
		setRequestBody(request, body)
		return nil
	}
	setRequestBody(request, modifiedBody)
	return nil
}

// ApplyGroupParamOverride applies a recursive group override and protects
// routing/transport-owned top-level fields.
func ApplyGroupParamOverride(request *http.Request, paramOverride *string) error {
	if request == nil || request.Body == nil || paramOverride == nil || strings.TrimSpace(*paramOverride) == "" {
		return nil
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return fmt.Errorf("failed to read request body: %w", err)
	}
	modifiedBody, changed, err := ApplyJSONParamOverride(body, paramOverride, true, true)
	if err != nil {
		setRequestBody(request, body)
		return err
	}
	if !changed {
		setRequestBody(request, body)
		return nil
	}
	setRequestBody(request, modifiedBody)
	return nil
}

// ApplyParamOverrides applies the channel rule first and the group rule last,
// making the group rule authoritative for overlapping business parameters.
func ApplyParamOverrides(request *http.Request, channelOverride, groupOverride *string) error {
	if err := ApplyParamOverride(request, channelOverride); err != nil {
		return err
	}
	return ApplyGroupParamOverride(request, groupOverride)
}

// ApplyJSONParamOverrides applies the same precedence to an already encoded
// JSON payload. It is used by WebSocket and special relay endpoints that do
// not represent their payload as *http.Request.
func ApplyJSONParamOverrides(body []byte, channelOverride, groupOverride *string) ([]byte, error) {
	result := append([]byte(nil), body...)
	if channelOverride != nil && strings.TrimSpace(*channelOverride) != "" {
		modified, changed, err := ApplyJSONParamOverride(result, channelOverride, false, false)
		if err != nil {
			// Keep channel compatibility: malformed legacy values are ignored.
		} else if changed {
			result = modified
		}
	}
	if groupOverride != nil && strings.TrimSpace(*groupOverride) != "" {
		modified, changed, err := ApplyJSONParamOverride(result, groupOverride, true, true)
		if err != nil {
			return result, err
		}
		if changed {
			result = modified
		}
	}
	return result, nil
}

// ApplyJSONParamOverride returns a JSON object with one override merged into
// it. Objects merge recursively when deep is true; all other JSON values are
// replaced as a whole. changed is false for empty/non-object request bodies.
func ApplyJSONParamOverride(body []byte, override *string, deep, protect bool) ([]byte, bool, error) {
	if override == nil || strings.TrimSpace(*override) == "" {
		return body, false, nil
	}
	var bodyMap map[string]json.RawMessage
	trimmedBody := bytes.TrimSpace(body)
	if len(trimmedBody) == 0 || trimmedBody[0] != '{' {
		return body, false, nil
	}
	if err := json.Unmarshal(trimmedBody, &bodyMap); err != nil {
		return body, false, nil
	}
	if bodyMap == nil {
		return body, false, nil
	}

	var overrideMap map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.TrimSpace(*override)), &overrideMap); err != nil {
		return body, false, fmt.Errorf("invalid param_override: %w", err)
	}
	if overrideMap == nil {
		return body, false, fmt.Errorf("param_override must be a JSON object")
	}
	if protect {
		for key := range overrideMap {
			if _, protected := protectedGroupOverrideKeys[strings.ToLower(strings.TrimSpace(key))]; protected {
				return body, false, fmt.Errorf("param_override cannot override protected field %q", key)
			}
		}
	}

	if err := mergeJSONObjects(bodyMap, overrideMap, deep, protect); err != nil {
		return body, false, err
	}
	modified, err := json.Marshal(bodyMap)
	if err != nil {
		return body, false, fmt.Errorf("failed to marshal request body with param override: %w", err)
	}
	return modified, true, nil
}

func mergeJSONObjects(dst, src map[string]json.RawMessage, deep, protect bool) error {
	for key, value := range src {
		if protect {
			if _, protected := protectedGroupOverrideKeys[strings.ToLower(strings.TrimSpace(key))]; protected {
				return fmt.Errorf("param_override cannot override protected field %q", key)
			}
		}
		if deep {
			var dstNested, srcNested map[string]json.RawMessage
			if json.Unmarshal(dst[key], &dstNested) == nil && dstNested != nil &&
				json.Unmarshal(value, &srcNested) == nil && srcNested != nil {
				if err := mergeJSONObjects(dstNested, srcNested, true, false); err != nil {
					return err
				}
				merged, err := json.Marshal(dstNested)
				if err != nil {
					return err
				}
				dst[key] = merged
				continue
			}
		}
		dst[key] = append(json.RawMessage(nil), value...)
	}
	return nil
}

func setRequestBody(request *http.Request, body []byte) {
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
}
