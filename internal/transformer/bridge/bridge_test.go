package bridge

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/looplj/axonhub/llm/httpclient"
)

func TestChunkFeederCloseUnblocksNext(t *testing.T) {
	feeder := NewChunkFeeder()
	nextDone := make(chan bool, 1)
	go func() {
		nextDone <- feeder.Next()
	}()

	select {
	case <-nextDone:
		t.Fatal("Next returned before feeder was closed")
	case <-time.After(20 * time.Millisecond):
	}

	if err := feeder.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	select {
	case ok := <-nextDone:
		if ok {
			t.Fatal("expected Next to return false after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Next")
	}
}

func TestChunkFeederFeedIsContextAware(t *testing.T) {
	feeder := NewChunkFeeder()
	if _, err := feeder.Feed(context.Background(), []byte("queued"), "message"); err != nil {
		t.Fatalf("queue first event: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := feeder.Feed(ctx, []byte("event"), "message"); err == nil {
		t.Fatal("expected canceled context to stop Feed")
	}
}

func TestBuildHTTPRequestDefaultsNilContext(t *testing.T) {
	req, err := BuildHTTPRequest(nil, &httpclient.Request{
		Method: http.MethodPost,
		URL:    "https://example.com/test",
	})
	if err != nil {
		t.Fatalf("BuildHTTPRequest(): %v", err)
	}
	if req == nil || req.URL.String() != "https://example.com/test" {
		t.Fatalf("unexpected request: %+v", req)
	}
}
