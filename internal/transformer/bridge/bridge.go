package bridge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

// ChunkFeeder is a pull stream with a one-event push boundary.  AxonHub's
// stream transformers pull from streams.Stream, while Octopus receives one
// SSE payload at a time from the relay.  Each event carries its own sequence
// number so acknowledgements cannot be confused when a stale notification is
// still buffered.
//
// The data channel is deliberately never closed.  Closing a channel while a
// concurrent Feed is selecting on it is a send-on-closed-channel race.  The
// separate done channel provides the same wake-up semantics without that
// hazard.
type ChunkFeeder struct {
	ch   chan feederEvent
	ack  chan uint64
	idle chan uint64
	done chan struct{}

	mu       sync.RWMutex
	closed   bool
	lastRead uint64
	seq      uint64
	current  *httpclient.StreamEvent

	closeOnce sync.Once
}

type feederEvent struct {
	seq   uint64
	event *httpclient.StreamEvent
}

func NewChunkFeeder() *ChunkFeeder {
	return &ChunkFeeder{
		ch:   make(chan feederEvent, 1),
		ack:  make(chan uint64, 1),
		idle: make(chan uint64, 1),
		done: make(chan struct{}),
	}
}

// Feed queues one event and waits if the stream has not pulled the preceding
// event yet.  It is context-aware, which is important when the client
// disconnects while the AxonHub transformer is blocked in Next.
func (f *ChunkFeeder) Feed(ctx context.Context, data []byte, eventType string) (uint64, error) {
	if f == nil {
		return 0, fmt.Errorf("nil chunk feeder")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	f.seq++
	seq := f.seq
	f.mu.Unlock()

	event := feederEvent{
		seq: seq,
		event: &httpclient.StreamEvent{
			Data: append([]byte(nil), data...),
			Type: eventType,
		},
	}
	select {
	case f.ch <- event:
		return seq, nil
	case <-f.done:
		return 0, io.ErrClosedPipe
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (f *ChunkFeeder) Ack() <-chan uint64 {
	if f == nil {
		return nil
	}
	return f.ack
}

func (f *ChunkFeeder) Idle() <-chan uint64 {
	if f == nil {
		return nil
	}
	return f.idle
}

// Next implements streams.Stream.  Idle is signalled immediately before the
// feeder waits for the next event; at that point AxonHub has fully processed
// the previous event and emitted every queued response.
func (f *ChunkFeeder) Next() bool {
	if f == nil {
		return false
	}

	f.mu.RLock()
	closed := f.closed
	lastRead := f.lastRead
	f.mu.RUnlock()
	if closed {
		return false
	}

	// Do not let a full notification channel stop the transformer goroutine.
	select {
	case f.idle <- lastRead:
	default:
		select {
		case <-f.idle:
		default:
		}
		select {
		case f.idle <- lastRead:
		default:
		}
	}

	select {
	case item := <-f.ch:
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			return false
		}
		f.current = item.event
		f.lastRead = item.seq
		f.mu.Unlock()
		select {
		case f.ack <- item.seq:
		default:
			select {
			case <-f.ack:
			default:
			}
			select {
			case f.ack <- item.seq:
			default:
			}
		}
		return true
	case <-f.done:
		return false
	}
}

func (f *ChunkFeeder) Current() *httpclient.StreamEvent {
	if f == nil {
		return nil
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.current
}

func (f *ChunkFeeder) Err() error { return nil }

func (f *ChunkFeeder) Close() error {
	if f == nil {
		return nil
	}
	f.closeOnce.Do(func() {
		f.mu.Lock()
		f.closed = true
		f.mu.Unlock()
		close(f.done)
	})
	return nil
}

// Drain is kept for callers which only need to wake a blocked Next.
func (f *ChunkFeeder) Drain() {
	if f != nil {
		_ = f.Close()
	}
}

// StreamFirst pulls the first available item from s.
func StreamFirst[T any](s streams.Stream[T]) (T, bool, error) {
	var zero T
	if s == nil || !s.Next() {
		if s == nil {
			return zero, false, nil
		}
		return zero, false, s.Err()
	}
	return s.Current(), true, s.Err()
}

// BuildHTTPRequest converts an AxonHub request into net/http and applies its
// structured authentication.  The previous bridge copied only Headers,
// which silently dropped the API key because AxonHub stores auth separately.
func BuildHTTPRequest(ctx context.Context, req *httpclient.Request) (*http.Request, error) {
	if req == nil {
		return nil, fmt.Errorf("empty outbound request")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	url := strings.TrimSpace(req.URL)
	if url == "" {
		return nil, fmt.Errorf("outbound request URL is empty")
	}
	var body io.Reader
	if len(req.Body) > 0 {
		body = bytes.NewReader(req.Body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, req.Method, url, body)
	if err != nil {
		return nil, fmt.Errorf("failed to build outbound request: %w", err)
	}
	if req.Headers != nil {
		httpReq.Header = req.Headers.Clone()
	}
	if req.Query != nil {
		parsed, parseErr := http.NewRequestWithContext(ctx, req.Method, url, nil)
		if parseErr == nil {
			values := parsed.URL.Query()
			for key, items := range req.Query {
				values.Del(key)
				for _, item := range items {
					values.Add(key, item)
				}
			}
			parsed.URL.RawQuery = values.Encode()
			httpReq.URL = parsed.URL
		}
	}
	if httpReq.Header.Get("Content-Type") == "" && req.ContentType != "" {
		httpReq.Header.Set("Content-Type", req.ContentType)
	}
	if req.Auth != nil && req.Auth.APIKey != "" {
		switch req.Auth.Type {
		case httpclient.AuthTypeAPIKey:
			header := req.Auth.HeaderKey
			if header == "" {
				header = "X-API-Key"
			}
			httpReq.Header.Set(header, req.Auth.APIKey)
		default:
			httpReq.Header.Set("Authorization", "Bearer "+req.Auth.APIKey)
		}
	}
	if len(req.Body) > 0 {
		httpReq.ContentLength = int64(len(req.Body))
		httpReq.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(req.Body)), nil
		}
	}
	// Preserve the complete AxonHub request for response transformers that use
	// provider metadata, request type, or the original raw body.
	httpReq = httpReq.WithContext(context.WithValue(httpReq.Context(), requestMetadataContextKey{}, req))
	return httpReq, nil
}

type requestMetadataContextKey struct{}

// RequestFromHTTPContext returns the structured AxonHub request attached by
// BuildHTTPRequest. It is intentionally small so relay code can preserve the
// metadata without depending on a concrete outbound transformer.
func RequestFromHTTPContext(req *http.Request) *httpclient.Request {
	if req == nil {
		return nil
	}
	value := req.Context().Value(requestMetadataContextKey{})
	metadata, _ := value.(*httpclient.Request)
	return metadata
}

// MergeURLQuery applies query values without discarding query parameters that
// were already present in the provider URL.
func MergeURLQuery(rawURL string, query url.Values) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	values := parsed.Query()
	for key, items := range query {
		values.Del(key)
		for _, item := range items {
			values.Add(key, item)
		}
	}
	parsed.RawQuery = values.Encode()
	return parsed.String(), nil
}
