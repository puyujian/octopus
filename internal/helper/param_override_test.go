package helper

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestApplyJSONParamOverridesPrecedenceAndDeepMerge(t *testing.T) {
	channel := `{"temperature":0.2,"nested":{"channel_only":true},"array":[2],"nullable":null}`
	group := `{"temperature":0.7,"nested":{"group_only":true},"array":[3],"nullable":"restored"}`
	body, err := ApplyJSONParamOverrides(
		[]byte(`{"temperature":1,"nested":{"original":true},"array":[1],"nullable":"value"}`),
		&channel,
		&group,
	)
	if err != nil {
		t.Fatalf("ApplyJSONParamOverrides returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode merged body: %v", err)
	}
	if got["temperature"] != float64(0.7) {
		t.Fatalf("group override did not win: %#v", got["temperature"])
	}
	array, ok := got["array"].([]any)
	if !ok || len(array) != 1 || array[0] != float64(3) {
		t.Fatalf("arrays should be replaced as a whole: %#v", got["array"])
	}
	if got["nullable"] != "restored" {
		t.Fatalf("scalar/null replacement failed: %#v", got["nullable"])
	}
	nested, ok := got["nested"].(map[string]any)
	if !ok || nested["channel_only"] != true || nested["group_only"] != true {
		t.Fatalf("expected recursive merge of channel and group objects: %#v", got["nested"])
	}
	if _, exists := nested["original"]; exists {
		t.Fatalf("channel shallow merge should replace the original nested object: %#v", nested)
	}
}

func TestApplyJSONParamOverridesProtectsTopLevelRoutingFields(t *testing.T) {
	group := `{"model":"forced","nested":{"model":"allowed"}}`
	body := []byte(`{"model":"actual","stream":true,"nested":{"model":"original"}}`)
	got, err := ApplyJSONParamOverrides(body, nil, &group)
	if err == nil || !strings.Contains(err.Error(), "protected field") {
		t.Fatalf("expected protected-field error, got %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("failed group override should leave body unchanged: %s", got)
	}

	allowed := `{"generation_config":{"thinking_config":{"thinking_level":"high"}}}`
	got, err = ApplyJSONParamOverrides(body, nil, &allowed)
	if err != nil {
		t.Fatalf("nested business override returned error: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["model"] != "actual" || decoded["stream"] != true {
		t.Fatalf("protected fields changed unexpectedly: %#v", decoded)
	}
}

func TestApplyParamOverrideRestoresRequestBody(t *testing.T) {
	override := `{"max_tokens":42}`
	req, err := http.NewRequest(http.MethodPost, "http://example.test", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyParamOverride(req, &override); err != nil {
		t.Fatal(err)
	}
	if req.ContentLength <= 0 || req.GetBody == nil {
		t.Fatalf("override did not refresh request body metadata")
	}
	body, err := req.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["max_tokens"] != float64(42) {
		t.Fatalf("unexpected request override: %#v", decoded)
	}
}

func TestValidateGroupParamOverride(t *testing.T) {
	for _, value := range []string{"", `{"temperature":0.5}`, ` {"nested":{"x":1}} `} {
		if err := ValidateGroupParamOverride(value); err != nil {
			t.Errorf("valid override %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{"[]", `{"MODEL":"m"}`, `{"stream":false}`, "not-json"} {
		if err := ValidateGroupParamOverride(value); err == nil {
			t.Errorf("invalid/protected override %q accepted", value)
		}
	}
}
