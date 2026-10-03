package sitesync

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

// Ordinary sites keep their existing request flow. Only an explicit missing-PoW
// response triggers the challenge flow; unrelated errors must not be retried.
func requestManagedCheckin(ctx context.Context, site *model.Site, account *model.SiteAccount, accessToken string) (map[string]any, error) {
	checkinURL := buildSiteURL(site.BaseURL, "/api/user/checkin")
	payload, err := requestJSONWithManagedAccessToken(ctx, site, http.MethodPost, checkinURL, nil, accessToken, account)
	message := jsonString(payload["message"])
	if err != nil {
		message = err.Error()
	}
	if (err == nil && jsonBool(payload["success"])) || !requiresCheckinPoW(message) {
		return payload, err
	}

	fallbackCtx, fallbackCancel := context.WithTimeout(ctx, 15*time.Second)
	defer fallbackCancel()
	challengePayload, err := requestJSONWithManagedAccessToken(fallbackCtx, site, http.MethodGet,
		buildSiteURL(site.BaseURL, "/api/user/pow/challenge?action=checkin"), nil, accessToken, account)
	if err != nil {
		return nil, fmt.Errorf("fetch checkin PoW challenge: %w", err)
	}
	if !jsonBool(challengePayload["success"]) {
		return nil, fmt.Errorf("fetch checkin PoW challenge: %s", firstNonEmptyString(jsonString(challengePayload["message"]), "challenge request failed"))
	}
	encoded, err := json.Marshal(challengePayload["data"])
	if err != nil {
		return nil, fmt.Errorf("invalid checkin PoW challenge: %w", err)
	}
	var challenge struct {
		ID         string `json:"challenge_id"`
		Prefix     string `json:"prefix"`
		Difficulty *int   `json:"difficulty"`
		ExpiresAt  int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(encoded, &challenge); err != nil {
		return nil, fmt.Errorf("invalid checkin PoW challenge: %w", err)
	}
	if strings.TrimSpace(challenge.ID) == "" || challenge.Prefix == "" || challenge.Difficulty == nil || *challenge.Difficulty < 0 || *challenge.Difficulty > 30 || challenge.ExpiresAt <= 0 {
		return nil, fmt.Errorf("invalid checkin PoW challenge parameters")
	}
	deadline := time.Now().Add(15 * time.Second)
	if challenge.ExpiresAt > 0 {
		// Leave time for the checkin request to reach the upstream before expiry.
		expires := time.Unix(challenge.ExpiresAt, 0).Add(-time.Second)
		if expires.Before(deadline) {
			deadline = expires
		}
	}
	powCtx, cancel := context.WithDeadline(fallbackCtx, deadline)
	defer cancel()
	nonce, err := solveCheckinPoW(powCtx, challenge.Prefix, *challenge.Difficulty)
	if err != nil {
		return nil, fmt.Errorf("solve checkin PoW challenge: %w", err)
	}
	query := url.Values{"pow_challenge": {challenge.ID}, "pow_nonce": {nonce}}
	return requestJSONWithManagedAccessToken(powCtx, site, http.MethodPost, checkinURL+"?"+query.Encode(), nil, accessToken, account)
}

func requiresCheckinPoW(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "pow") && strings.Contains(lower, "challenge") &&
		strings.Contains(lower, "nonce") && (strings.Contains(lower, "required") || strings.Contains(lower, "missing"))
}

// Match the upstream worker exactly: SHA256(prefix + eight-digit lowercase hex
// nonce) must start with difficulty zero bits, not zero hexadecimal characters.
func solveCheckinPoW(ctx context.Context, prefix string, difficulty int) (string, error) {
	if prefix == "" || difficulty < 0 || difficulty > 30 {
		return "", fmt.Errorf("invalid PoW parameters")
	}
	input := make([]byte, len(prefix)+8)
	copy(input, prefix)
	const hexDigits = "0123456789abcdef"
	for attempt := uint64(0); attempt <= 0xffffffff; attempt++ {
		if attempt%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		value := attempt
		for i := len(input) - 1; i >= len(prefix); i-- {
			input[i] = hexDigits[value&15]
			value >>= 4
		}
		digest := sha256.Sum256(input)
		if checkinPoWHasZeroBits(digest, difficulty) {
			return string(input[len(prefix):]), nil
		}
	}
	return "", fmt.Errorf("PoW nonce space exhausted")
}

func checkinPoWHasZeroBits(digest [sha256.Size]byte, difficulty int) bool {
	if difficulty < 0 || difficulty > sha256.Size*8 {
		return false
	}
	whole, remaining := difficulty/8, difficulty%8
	for i := 0; i < whole; i++ {
		if digest[i] != 0 {
			return false
		}
	}
	return remaining == 0 || digest[whole]>>(8-remaining) == 0
}
