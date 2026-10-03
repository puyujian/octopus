package sitesync

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bestruirui/octopus/internal/model"
)

func TestSolveCheckinPoW(t *testing.T) {
	for _, difficulty := range []int{0, 1, 7, 8, 9, 14} {
		nonce, err := solveCheckinPoW(context.Background(), "checkin:test:", difficulty)
		if err != nil || len(nonce) != 8 || strings.Trim(nonce, "0123456789abcdef") != "" {
			t.Fatalf("difficulty=%d nonce=%q err=%v", difficulty, nonce, err)
		}
		if !checkinPoWHasZeroBits(sha256.Sum256([]byte("checkin:test:"+nonce)), difficulty) {
			t.Fatalf("invalid nonce at difficulty %d", difficulty)
		}
		if difficulty == 0 && nonce != "00000000" {
			t.Fatalf("zero-difficulty nonce = %q", nonce)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := solveCheckinPoW(ctx, "prefix", 14); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	for _, difficulty := range []int{-1, 31} {
		if _, err := solveCheckinPoW(context.Background(), "prefix", difficulty); err == nil {
			t.Fatalf("accepted difficulty %d", difficulty)
		}
	}
}

func TestCheckinPoWZeroBits(t *testing.T) {
	var digest [sha256.Size]byte
	digest[1] = 0x40
	if !checkinPoWHasZeroBits(digest, 9) || checkinPoWHasZeroBits(digest, 10) {
		t.Fatal("incorrect non-byte-aligned bit check")
	}
	if checkinPoWHasZeroBits(digest, -1) || checkinPoWHasZeroBits(digest, 257) {
		t.Fatal("accepted invalid bit count")
	}
}

func TestManagedCheckinPoWFallback(t *testing.T) {
	for _, httpError := range []bool{false, true} {
		t.Run(fmt.Sprintf("http_error_%v", httpError), func(t *testing.T) {
			checkinCalls, challengeCalls := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Header.Get("Authorization") != "Bearer test-access-token" || r.Header.Get("New-API-User") != "3332" {
					t.Error("missing managed authentication headers")
				}
				switch r.URL.Path {
				case "/api/user/pow/challenge":
					challengeCalls++
					if r.Method != http.MethodGet || r.URL.Query().Get("action") != "checkin" {
						t.Error("wrong challenge request")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
						"challenge_id": "challenge+with&reserved", "prefix": "checkin:test:", "difficulty": 9, "expires_at": time.Now().Add(20 * time.Second).Unix(),
					}})
				case "/api/user/checkin":
					checkinCalls++
					if r.Method != http.MethodPost {
						t.Error("wrong checkin method")
					}
					if checkinCalls == 1 {
						if httpError {
							w.WriteHeader(http.StatusBadRequest)
						}
						_, _ = w.Write([]byte(`{"success":false,"message":"PoW challenge and nonce are required"}`))
						return
					}
					nonce := r.URL.Query().Get("pow_nonce")
					if r.URL.Query().Get("pow_challenge") != "challenge+with&reserved" || len(nonce) != 8 || !checkinPoWHasZeroBits(sha256.Sum256([]byte("checkin:test:"+nonce)), 9) {
						t.Error("invalid PoW query parameters")
					}
					_, _ = w.Write([]byte(`{"success":true,"message":"签到成功","data":{"quota_awarded":12500000}}`))
				default:
					t.Errorf("unexpected request %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			result, _, err := checkinAccountState(context.Background(), &model.Site{Platform: model.SitePlatformNewAPI, BaseURL: server.URL}, &model.SiteAccount{
				CredentialType: model.SiteCredentialTypeAccessToken, AccessToken: "test-access-token", PlatformUserID: new(3332),
			})
			if err != nil || result == nil || result.Status != model.SiteExecutionStatusSuccess || checkinCalls != 2 || challengeCalls != 1 {
				t.Fatalf("result=%+v err=%v checkins=%d challenges=%d", result, err, checkinCalls, challengeCalls)
			}
		})
	}
}

func TestManagedCheckinDoesNotRetryOrdinaryResponses(t *testing.T) {
	for _, response := range []string{
		`{"success":true,"message":"签到成功"}`,
		`{"success":false,"message":"今天已签到"}`,
		`{"success":false,"message":"签到功能未启用"}`,
		`{"success":false,"message":"Turnstile token 为空"}`,
		`{"success":false,"message":"PoW verification failed"}`,
	} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(response))
		}))
		payload, err := requestManagedCheckin(context.Background(), &model.Site{BaseURL: server.URL}, &model.SiteAccount{}, "token")
		server.Close()
		if err != nil || calls != 1 || payload == nil {
			t.Fatalf("response=%s calls=%d err=%v", response, calls, err)
		}
	}
}

func TestManagedCheckinRejectsBadChallenges(t *testing.T) {
	for _, data := range []string{
		`{"challenge_id":"id","prefix":"prefix","difficulty":31}`,
		`{"challenge_id":"id","prefix":"prefix"}`,
		`{"challenge_id":"id","prefix":"prefix","difficulty":1.5}`,
		`{"challenge_id":"id","prefix":"prefix","difficulty":0,"expires_at":1}`,
		`{"challenge_id":"","prefix":"prefix","difficulty":14}`,
	} {
		checkins := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/api/user/checkin" {
				checkins++
				_, _ = w.Write([]byte(`{"success":false,"message":"PoW challenge and nonce are required"}`))
			} else {
				_, _ = fmt.Fprintf(w, `{"success":true,"data":%s}`, data)
			}
		}))
		_, err := requestManagedCheckin(context.Background(), &model.Site{BaseURL: server.URL}, &model.SiteAccount{}, "token")
		server.Close()
		if err == nil || checkins != 1 {
			t.Fatalf("bad challenge=%s checkins=%d err=%v", data, checkins, err)
		}
	}
}
