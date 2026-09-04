package relay

import "github.com/bestruirui/octopus/internal/helper"

// unsupportedUpstreamParameter extracts only explicit parameter-rejection
// messages. It intentionally does not infer from generic 400s, so compatibility
// retries cannot hide unrelated request or authentication failures.
func unsupportedUpstreamParameter(body []byte) string {
	return helper.RejectedUpstreamParameter(body)
}
