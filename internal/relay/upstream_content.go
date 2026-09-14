package relay

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Reject maintenance/login/gateway pages even when they arrive with HTTP 200
// or a misleading JSON content type. Keep the body and its Close hook intact
// for valid responses and for the caller's normal cleanup.
func rejectUpstreamHTML(response *http.Response) error {
	if response == nil || response.Body == nil {
		return nil
	}
	prefix := make([]byte, 512)
	n, err := io.ReadFull(response.Body, prefix)
	prefix = prefix[:n]
	original := response.Body
	response.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(prefix), original), original}
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return fmt.Errorf("failed to read upstream response: %w", err)
	}
	contentType := response.Header.Get("Content-Type")
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(prefix, []byte{0xef, 0xbb, 0xbf}))
	if strings.Contains(strings.ToLower(contentType), "text/html") || bytes.HasPrefix(trimmed, []byte("<")) {
		return fmt.Errorf("upstream returned HTML/XML instead of JSON (status=%d, content-type=%q); possible maintenance or gateway page", response.StatusCode, contentType)
	}
	return nil
}
