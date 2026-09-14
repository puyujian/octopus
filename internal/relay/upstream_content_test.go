package relay

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type contentTestBody struct {
	io.Reader
	closed bool
}

func (b *contentTestBody) Close() error { b.closed = true; return nil }

func TestRejectUpstreamHTMLPreservesJSONAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body string
		reject                  bool
	}{
		{"maintenance", "text/html", "<!DOCTYPE html><html>Maintenance</html>", true},
		{"mislabelled page", "application/json", "  <html>Login</html>", true},
		{"BOM page", "", "\xef\xbb\xbf<html>Unavailable</html>", true},
		{"JSON", "application/json", `{"output":[{"text":"<html> is text"}]}`, false},
		{"long JSON", "", `{"text":"` + strings.Repeat("a", 1024) + `"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := &contentTestBody{Reader: strings.NewReader(tc.body)}
			response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{tc.contentType}}, Body: original}
			err := rejectUpstreamHTML(response)
			if (err != nil) != tc.reject {
				t.Fatalf("unexpected result: %v", err)
			}
			body, _ := io.ReadAll(response.Body)
			if string(body) != tc.body || original.closed {
				t.Fatal("body consumed/closed before caller")
			}
			_ = response.Body.Close()
			if !original.closed {
				t.Fatal("Close hook lost")
			}
		})
	}
}
