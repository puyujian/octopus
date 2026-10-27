package relay

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/relay/balancer"
	transformerModel "github.com/bestruirui/octopus/internal/transformer/model"
	"github.com/bestruirui/octopus/internal/utils/log"
)

const (
	responsesReplayStoreMaxEntries    = 10000
	responsesReplayStoreMaxSize       = 100 * 1024 * 1024
	responsesReplayStoreSweepInterval = 5 * time.Minute
)

// Entries are immutable after insertion. A single lock protects both the LRU
// order and byte accounting, including replacements and concurrent expiry.
type responsesReplayStateEntry struct {
	key       string
	state     *wsConversationState
	expiresAt time.Time
	size      int
}

type responsesReplayCache struct {
	mu         sync.Mutex
	entries    map[string]*list.Element
	order      list.List // most recently used first
	totalSize  int
	maxEntries int
	maxSize    int
}

func newResponsesReplayCache(maxEntries, maxSize int) *responsesReplayCache {
	return &responsesReplayCache{
		entries:    make(map[string]*list.Element),
		maxEntries: maxEntries,
		maxSize:    maxSize,
	}
}

var responsesReplayStore = newResponsesReplayCache(responsesReplayStoreMaxEntries, responsesReplayStoreMaxSize)

func (c *responsesReplayCache) removeLocked(element *list.Element) {
	entry := element.Value.(*responsesReplayStateEntry)
	delete(c.entries, entry.key)
	c.order.Remove(element)
	c.totalSize -= entry.size
}

func (c *responsesReplayCache) sweepLocked(now time.Time) int {
	removed := 0
	for _, element := range c.entries {
		entry := element.Value.(*responsesReplayStateEntry)
		if !now.Before(entry.expiresAt) {
			c.removeLocked(element)
			removed++
		}
	}
	return removed
}

func (c *responsesReplayCache) load(key string) *wsConversationState {
	c.mu.Lock()
	element := c.entries[key]
	if element == nil {
		c.mu.Unlock()
		return nil
	}
	entry := element.Value.(*responsesReplayStateEntry)
	if !time.Now().Before(entry.expiresAt) {
		c.removeLocked(element)
		c.mu.Unlock()
		return nil
	}
	c.order.MoveToFront(element)
	c.mu.Unlock()
	// The immutable snapshot remains valid if it is evicted while cloning.
	return cloneWSConversationState(entry.state)
}

func (c *responsesReplayCache) store(key string, state *wsConversationState, ttl time.Duration) bool {
	size := estimateStateSize(state) + len(key)
	// Reject an individually oversized state before evicting anything. Never
	// silently trim conversation history or discard an existing usable value.
	if size > c.maxSize || c.maxEntries < 1 || state == nil {
		return false
	}
	entry := &responsesReplayStateEntry{
		key: key, state: cloneWSConversationState(state), size: size,
		expiresAt: time.Now().Add(ttl),
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	oldSize, added := 0, 1
	if old := c.entries[key]; old != nil {
		oldSize = old.Value.(*responsesReplayStateEntry).size
		added = 0
	}
	if len(c.entries)+added > c.maxEntries || c.totalSize-oldSize+size > c.maxSize {
		// Prefer expired entries over evicting a still usable conversation.
		c.sweepLocked(time.Now())
	}
	if old := c.entries[key]; old != nil {
		c.removeLocked(old)
	}
	for len(c.entries) >= c.maxEntries || c.totalSize+size > c.maxSize {
		c.removeLocked(c.order.Back())
	}
	c.entries[key] = c.order.PushFront(entry)
	c.totalSize += size
	return true
}

func init() {
	startResponsesReplayStoreSweeper()
}

var responsesReplayStoreSweepStop = make(chan struct{})

func startResponsesReplayStoreSweeper() {
	go func() {
		ticker := time.NewTicker(responsesReplayStoreSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sweepExpiredResponsesReplayStates()
			case <-responsesReplayStoreSweepStop:
				return
			}
		}
	}()
}

func sweepExpiredResponsesReplayStates() {
	responsesReplayStore.mu.Lock()
	removed := responsesReplayStore.sweepLocked(time.Now())
	entries, size := len(responsesReplayStore.entries), responsesReplayStore.totalSize
	responsesReplayStore.mu.Unlock()
	if removed > 0 {
		log.Debugf("HTTP replay store sweep: removed %d expired entries, current entries=%d, size=%d", removed, entries, size)
	}
}

func responsesReplayStateKey(apiKeyID, groupID int, requestModel, responseID string) string {
	requestModel = strings.TrimSpace(requestModel)
	responseID = strings.TrimSpace(responseID)
	if requestModel == "" || responseID == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(responseID))
	hashStr := hex.EncodeToString(hash[:])[:32]
	return fmt.Sprintf("%d:%d:%s:%s", apiKeyID, groupID, requestModel, hashStr)
}

func loadResponsesReplayState(apiKeyID, groupID int, requestModel, responseID string) *wsConversationState {
	key := responsesReplayStateKey(apiKeyID, groupID, requestModel, responseID)
	if key == "" {
		return nil
	}

	return responsesReplayStore.load(key)
}

func storeResponsesReplayState(apiKeyID, groupID int, requestModel string, state *wsConversationState, ttl time.Duration) {
	requestModel = strings.TrimSpace(requestModel)
	if requestModel == "" || state == nil {
		return
	}
	responseID := strings.TrimSpace(state.LastResponseID)
	if responseID == "" {
		return
	}
	if ttl <= 0 {
		ttl = wsClientMaxAge
	}

	key := responsesReplayStateKey(apiKeyID, groupID, requestModel, responseID)
	if key == "" {
		return
	}

	cloned := *state
	cloned.RequestModel = requestModel

	if !responsesReplayStore.store(key, &cloned, ttl) {
		log.Warnf("HTTP replay state exceeds cache capacity (size=%d, limit=%d), skipping save", estimateStateSize(&cloned)+len(key), responsesReplayStoreMaxSize)
	}
}

func estimateStateSize(state *wsConversationState) int {
	if state == nil {
		return 0
	}
	size := 256
	size += len(state.DownstreamSessionID) + len(state.RequestModel) + len(state.LastResponseID)
	size += len(state.ReplayWindowItems)
	for _, msg := range state.Transcript {
		size += estimateMessageSize(msg)
	}
	for _, alias := range state.ReplayAliases {
		size += len(alias) + 16
	}
	return size
}

func estimateMessageSize(msg transformerModel.Message) int {
	size := 128
	if msg.Content.Content != nil {
		size += len(*msg.Content.Content)
	}
	for _, part := range msg.Content.MultipleContent {
		size += 64
		if part.Text != nil {
			size += len(*part.Text)
		}
		if part.ImageURL != nil {
			size += len(part.ImageURL.URL)
		}
	}
	for _, tc := range msg.ToolCalls {
		size += len(tc.ID) + len(tc.Function.Name) + len(tc.Function.Arguments) + 32
	}
	if msg.ToolCallID != nil {
		size += len(*msg.ToolCallID)
	}
	if msg.ReasoningContent != nil {
		size += len(*msg.ReasoningContent)
	}
	for _, img := range msg.Images {
		size += 64
		if img.Text != nil {
			size += len(*img.Text)
		}
		if img.ImageURL != nil {
			size += len(img.ImageURL.URL)
		}
	}
	return size
}

func resolveResponsesReplayState(apiKeyID, groupID int, requestModel string, req *transformerModel.InternalLLMRequest) *wsConversationState {
	requestModel = strings.TrimSpace(requestModel)
	if requestModel == "" || req == nil {
		return nil
	}
	prevID := req.OpenAIPreviousResponseID()
	if prevID == "" {
		return nil
	}
	return loadResponsesReplayState(apiKeyID, groupID, requestModel, prevID)
}

func responsesReplayStateToSticky(state *wsConversationState) *balancer.SessionEntry {
	if state == nil || state.ChannelID <= 0 {
		return nil
	}
	return &balancer.SessionEntry{
		ChannelID:    state.ChannelID,
		ChannelKeyID: state.ChannelKeyID,
		Timestamp:    time.Now(),
	}
}

func resetResponsesReplayStore() {
	responsesReplayStore.mu.Lock()
	defer responsesReplayStore.mu.Unlock()
	responsesReplayStore.entries = make(map[string]*list.Element)
	responsesReplayStore.order.Init()
	responsesReplayStore.totalSize = 0
}
