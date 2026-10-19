package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"subscription-mediator/internal/config"
	"subscription-mediator/internal/gateway"
	"subscription-mediator/internal/mediator"
	"subscription-mediator/internal/webhook"
)

const testSecret = "a-webhook-subscriber-secret-at-least-32-chars"

// newTestServer builds the server against a gateway that answers every call
// with gatewayStatus.
func newTestServer(t *testing.T, gatewayStatus int) http.Handler {
	t.Helper()
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(gatewayStatus)
		_, _ = w.Write([]byte(`{"subscriptions":[],"message":"stub"}`))
	}))
	t.Cleanup(gw.Close)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	codec, err := webhook.NewCodec(testSecret)
	if err != nil {
		t.Fatalf("webhook.NewCodec() = %v", err)
	}
	client, err := gateway.New(gw.URL, "foo", "bar", "")
	if err != nil {
		t.Fatalf("gateway.New() = %v", err)
	}
	med := mediator.New(client, codec, map[string]string{}, logger)
	return New(codec, med, logger).Handler()
}

func event(t *testing.T, eventType string, data map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"event_id": "evt-1", "event_type": eventType,
		"occurred_at":      time.Now().UTC().Format(time.RFC3339),
		"org":              map[string]string{"ref_id": "cp-org-1"},
		"encrypted_fields": []string{},
		"data":             data,
	})
	if err != nil {
		t.Fatalf("marshalling the event: %v", err)
	}
	return body
}

// deliver posts a body the way the API Portal does, signed unless signature is
// given.
func deliver(t *testing.T, h http.Handler, body []byte, signature string) *httptest.ResponseRecorder {
	t.Helper()
	if signature == "" {
		ts := time.Now().Unix()
		mac := hmac.New(sha256.New, []byte(testSecret))
		mac.Write([]byte(strconv.FormatInt(ts, 10) + "." + string(body)))
		signature = fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
	}
	req := httptest.NewRequest(http.MethodPost, config.WebhookPath, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Portal-Signature", signature)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAppliedEventAnswers200(t *testing.T) {
	h := newTestServer(t, http.StatusOK)
	rec := deliver(t, h, event(t, "application.created", map[string]any{"application_id": "app-1"}), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] != "processed" || got["eventId"] != "evt-1" {
		t.Errorf("body = %v, want status processed for evt-1", got)
	}
}

func TestUnsignedDeliveryAnswers401(t *testing.T) {
	h := newTestServer(t, http.StatusOK)
	rec := deliver(t, h, event(t, "application.created", map[string]any{}), "t=1,v1=00")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// TestEventThatCannotApplyAnswers422 covers an event no retry can fix, here a
// status change that carries no status.
func TestEventThatCannotApplyAnswers422(t *testing.T) {
	h := newTestServer(t, http.StatusOK)
	rec := deliver(t, h, event(t, webhook.EventSubscriptionUpdated, map[string]any{"subscription_id": "sub-1"}), "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body)
	}
}

func TestGatewayFailureAnswers502(t *testing.T) {
	h := newTestServer(t, http.StatusServiceUnavailable)
	rec := deliver(t, h, event(t, webhook.EventSubscriptionDeleted, map[string]any{"subscription_id": "sub-1"}), "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body %s", rec.Code, rec.Body)
	}
}

func TestStatusCountsAppliedAndFailedEvents(t *testing.T) {
	h := newTestServer(t, http.StatusOK)
	deliver(t, h, event(t, "application.created", map[string]any{}), "")
	deliver(t, h, event(t, webhook.EventSubscriptionUpdated, map[string]any{"subscription_id": "sub-1"}), "")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	var got struct {
		Events struct {
			Applied int64 `json:"applied"`
			Failed  int64 `json:"failed"`
		} `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding /status: %v", err)
	}
	if got.Events.Applied != 1 || got.Events.Failed != 1 {
		t.Errorf("/status = %+v, want 1 applied and 1 failed", got.Events)
	}
}
