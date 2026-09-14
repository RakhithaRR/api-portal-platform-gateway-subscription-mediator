package mediator

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha3"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"subscription-mediator/internal/gateway"
	"subscription-mediator/internal/store"
	"subscription-mediator/internal/webhook"
)

const (
	testSecret = "a-webhook-subscriber-secret-at-least-32-chars"
	testToken  = "sub-token-3f9a1c7e-5b2d-4e81-9a03-6c4f7b1e8d25"
)

// fakeGateway stands in for the Gateway Controller management REST API, so the
// tests assert on the translation rather than on the mediator's own calls.
type fakeGateway struct {
	mu    sync.Mutex
	apis  []gateway.RestAPI
	plans map[string]*gateway.SubscriptionPlan
	subs  map[string]*gateway.Subscription
	keys  map[string]*gateway.APIKey // "<apiHandle>/<keyName>"

	// failCreateTimes makes the next N subscription creates fail with a 503, so
	// retry behaviour is testable.
	failCreateTimes int
	nextID          int
}

func newFakeGateway() *fakeGateway {
	return &fakeGateway{
		plans: map[string]*gateway.SubscriptionPlan{},
		subs:  map[string]*gateway.Subscription{},
		keys:  map[string]*gateway.APIKey{},
	}
}

func (f *fakeGateway) addAPI(displayName, version, handle string, planNames ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var api gateway.RestAPI
	api.Metadata.Name = handle
	api.Spec.DisplayName = displayName
	api.Spec.Version = version
	api.Spec.SubscriptionPlans = planNames
	api.Status.ID = handle
	f.apis = append(f.apis, api)
}

// setAPIPlans rewrites an API's plan allowlist, standing in for an operator
// widening it on the gateway with no portal event to announce the change.
func (f *fakeGateway) setAPIPlans(handle string, planNames ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.apis {
		if f.apis[i].Handle() == handle {
			f.apis[i].Spec.SubscriptionPlans = planNames
			return
		}
	}
}

func (f *fakeGateway) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s-%d", prefix, f.nextID)
}

func (f *fakeGateway) start(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/management/v1/rest-apis", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		name, version := r.URL.Query().Get("displayName"), r.URL.Query().Get("version")
		out := make([]gateway.RestAPI, 0, len(f.apis))
		for _, api := range f.apis {
			if (name == "" || api.Spec.DisplayName == name) && (version == "" || api.Spec.Version == version) {
				out = append(out, api)
			}
		}
		writeJSON(w, 200, map[string]any{"count": len(out), "apis": out})
	})

	mux.HandleFunc("GET /api/management/v1/subscription-plans", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		out := make([]gateway.SubscriptionPlan, 0, len(f.plans))
		for _, p := range f.plans {
			out = append(out, *p)
		}
		writeJSON(w, 200, map[string]any{"subscriptionPlans": out})
	})

	mux.HandleFunc("POST /api/management/v1/subscription-plans", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		decode(t, r, &req)
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, p := range f.plans {
			if strings.EqualFold(p.PlanName, req["planName"]) {
				writeJSON(w, 409, map[string]string{"message": "Subscription plan already exists"})
				return
			}
		}
		plan := &gateway.SubscriptionPlan{ID: f.id("plan"), PlanName: req["planName"], Status: "ACTIVE"}
		f.plans[plan.ID] = plan
		writeJSON(w, 201, plan)
	})

	mux.HandleFunc("GET /api/management/v1/subscription-plans/{planId}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		plan, ok := f.plans[r.PathValue("planId")]
		if !ok {
			writeJSON(w, 404, map[string]string{"message": "Subscription plan not found"})
			return
		}
		writeJSON(w, 200, plan)
	})

	mux.HandleFunc("POST /api/management/v1/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		var req gateway.SubscriptionCreateRequest
		decode(t, r, &req)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.failCreateTimes > 0 {
			f.failCreateTimes--
			writeJSON(w, 503, map[string]string{"message": "Gateway is starting up"})
			return
		}
		// Mirror the gateway: reject a plan the API's allowlist excludes.
		if req.SubscriptionPlanID != "" {
			for _, api := range f.apis {
				if api.Handle() != req.APIID || len(api.Spec.SubscriptionPlans) == 0 {
					continue
				}
				allowed := false
				for _, name := range api.Spec.SubscriptionPlans {
					if plan, ok := f.plans[req.SubscriptionPlanID]; ok && strings.EqualFold(plan.PlanName, name) {
						allowed = true
					}
				}
				if !allowed {
					writeJSON(w, 400, map[string]string{"message": `Subscription plan is not enabled for this API`})
					return
				}
			}
		}
		for _, sub := range f.subs {
			if sub.APIID == req.APIID && sub.ApplicationID == req.ApplicationID {
				writeJSON(w, 409, map[string]string{"message": "Application already subscribed to this API"})
				return
			}
		}
		sub := &gateway.Subscription{
			ID: f.id("sub"), APIID: req.APIID, ApplicationID: req.ApplicationID,
			SubscriptionPlanID: req.SubscriptionPlanID, Status: req.Status,
		}
		f.subs[sub.ID] = sub
		// The token is not part of the gateway's list/get shape, so keep it aside.
		f.tokens()[sub.ID] = req.SubscriptionToken
		writeJSON(w, 201, sub)
	})

	mux.HandleFunc("GET /api/management/v1/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		apiID, appID := r.URL.Query().Get("apiId"), r.URL.Query().Get("applicationId")
		out := make([]gateway.Subscription, 0, len(f.subs))
		for _, sub := range f.subs {
			if (apiID == "" || sub.APIID == apiID) && (appID == "" || sub.ApplicationID == appID) {
				out = append(out, *sub)
			}
		}
		writeJSON(w, 200, map[string]any{"subscriptions": out})
	})

	mux.HandleFunc("PUT /api/management/v1/subscriptions/{subscriptionId}", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]string
		decode(t, r, &req)
		f.mu.Lock()
		defer f.mu.Unlock()
		sub, ok := f.subs[r.PathValue("subscriptionId")]
		if !ok {
			writeJSON(w, 404, map[string]string{"message": "Subscription not found"})
			return
		}
		if req["status"] != "" {
			sub.Status = req["status"]
		}
		writeJSON(w, 200, sub)
	})

	mux.HandleFunc("DELETE /api/management/v1/subscriptions/{subscriptionId}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("subscriptionId")
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.subs[id]; !ok {
			writeJSON(w, 404, map[string]string{"message": "Subscription not found"})
			return
		}
		delete(f.subs, id)
		w.WriteHeader(204)
	})

	mux.HandleFunc("POST /api/management/v1/rest-apis/{id}/api-keys", func(w http.ResponseWriter, r *http.Request) {
		var req gateway.APIKeyRequest
		decode(t, r, &req)
		apiHandle := r.PathValue("id")
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.hasAPILocked(apiHandle) {
			writeJSON(w, 404, map[string]string{"message": "RestAPI not found"})
			return
		}
		mapKey := apiHandle + "/" + req.Name
		if _, exists := f.keys[mapKey]; exists {
			writeJSON(w, 409, map[string]string{"message": "API key already exists"})
			return
		}
		key := &gateway.APIKey{Name: req.Name, APIID: apiHandle, Status: "active"}
		f.keys[mapKey] = key
		f.keyValues()[mapKey] = req.APIKey
		writeJSON(w, 201, map[string]any{"apiKey": key})
	})

	mux.HandleFunc("PUT /api/management/v1/rest-apis/{id}/api-keys/{apiKeyName}", func(w http.ResponseWriter, r *http.Request) {
		var req gateway.APIKeyRequest
		decode(t, r, &req)
		mapKey := r.PathValue("id") + "/" + r.PathValue("apiKeyName")
		f.mu.Lock()
		defer f.mu.Unlock()
		key, ok := f.keys[mapKey]
		if !ok {
			writeJSON(w, 404, map[string]string{"message": "API key not found"})
			return
		}
		f.keyValues()[mapKey] = req.APIKey
		writeJSON(w, 200, map[string]any{"apiKey": key})
	})

	mux.HandleFunc("DELETE /api/management/v1/rest-apis/{id}/api-keys/{apiKeyName}", func(w http.ResponseWriter, r *http.Request) {
		mapKey := r.PathValue("id") + "/" + r.PathValue("apiKeyName")
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.keys[mapKey]; !ok {
			writeJSON(w, 404, map[string]string{"message": "API key not found"})
			return
		}
		delete(f.keys, mapKey)
		w.WriteHeader(204)
	})

	// Every request must arrive with the configured basic credentials.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "foo" || pass != "bar" {
			writeJSON(w, 401, map[string]string{"message": "Unauthorized"})
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/api/management/v1"
}

var (
	tokenStore    = map[string]string{}
	keyValueStore = map[string]string{}
	sideMu        sync.Mutex
)

func (f *fakeGateway) tokens() map[string]string    { return tokenStore }
func (f *fakeGateway) keyValues() map[string]string { return keyValueStore }

func (f *fakeGateway) hasAPILocked(handle string) bool {
	for _, api := range f.apis {
		if api.Handle() == handle {
			return true
		}
	}
	return false
}

func (f *fakeGateway) subCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

func (f *fakeGateway) onlySub(t *testing.T) (gateway.Subscription, string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.subs) != 1 {
		t.Fatalf("the gateway holds %d subscriptions, want exactly 1", len(f.subs))
	}
	for _, sub := range f.subs {
		return *sub, tokenStore[sub.ID]
	}
	return gateway.Subscription{}, ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decode(t *testing.T, r *http.Request, out any) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		return
	}
	if err := json.Unmarshal(body, out); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
}

// --- harness ---------------------------------------------------------------

type harness struct {
	med   *Mediator
	store *store.Store
	fake  *fakeGateway
}

func newHarness(t *testing.T, fake *fakeGateway, overrides map[string]string) *harness {
	t.Helper()
	sideMu.Lock()
	tokenStore = map[string]string{}
	keyValueStore = map[string]string{}
	sideMu.Unlock()

	baseURL := fake.start(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	st, err := store.Open(filepath.Join(t.TempDir(), "state.jsonl"), 10000)
	if err != nil {
		t.Fatalf("store.Open() = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	codec, err := webhook.NewCodec(testSecret)
	if err != nil {
		t.Fatalf("webhook.NewCodec() = %v", err)
	}
	if overrides == nil {
		overrides = map[string]string{}
	}
	client := gateway.New(baseURL, "foo", "bar")
	return &harness{med: New(client, st, codec, overrides, logger), store: st, fake: fake}
}

// applyNow applies one event synchronously, so a test asserts on a completed
// translation rather than racing the dispatcher.
func (h *harness) applyNow(t *testing.T, body []byte) error {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return h.med.apply(context.Background(), &store.Pending{Body: body}, logger)
}

func encryptField(t *testing.T, plaintext string) map[string]string {
	t.Helper()
	key, err := hkdf.Key(sha3.New256, []byte(testSecret), nil, "api-portal-webhook-field-encryption-v1", 32)
	if err != nil {
		t.Fatalf("hkdf.Key() = %v", err)
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		t.Fatalf("rand.Read() = %v", err)
	}
	sealed := gcm.Seal(nil, iv, []byte(plaintext), nil)
	return map[string]string{
		"iv":         base64.StdEncoding.EncodeToString(iv),
		"tag":        base64.StdEncoding.EncodeToString(sealed[len(sealed)-16:]),
		"ciphertext": base64.StdEncoding.EncodeToString(sealed[:len(sealed)-16]),
	}
}

func apiRef() map[string]any {
	return map[string]any{"ref_id": "cp-api-1", "name": "Order API", "version": "v1.0", "type": "RestApi"}
}

func buildEvent(t *testing.T, id, eventType string, data map[string]any, encrypted ...string) []byte {
	t.Helper()
	if encrypted == nil {
		encrypted = []string{}
	}
	body, err := json.Marshal(map[string]any{
		"event_id": id, "event_type": eventType,
		"occurred_at":      time.Now().UTC().Format(time.RFC3339),
		"org":              map[string]string{"ref_id": "cp-org-1"},
		"encrypted_fields": encrypted,
		"data":             data,
	})
	if err != nil {
		t.Fatalf("marshalling the event: %v", err)
	}
	return body
}

func createdEvent(t *testing.T, id string) []byte {
	return buildEvent(t, id, webhook.EventSubscriptionCreated, map[string]any{
		"subscription_id": "portal-sub-1", "subscriber_id": "dev@example.com", "status": "ACTIVE",
		"application_id":    "portal-app-1",
		"subscription_plan": map[string]string{"ref_id": "portal-plan-gold", "name": "Gold"},
		"api":               apiRef(), "token": encryptField(t, testToken),
	}, "token")
}

// --- tests -----------------------------------------------------------------

// TestSubscriptionCreatedBecomesAGatewaySubscription is the core translation: a
// portal event resolved onto an APIM-deployed gateway API by display name and
// version, with the portal's own token injected.
func TestSubscriptionCreatedBecomesAGatewaySubscription(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)

	if err := h.applyNow(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("apply(subscription.created) = %v", err)
	}

	sub, token := fake.onlySub(t)
	if sub.APIID != "order-api-v1.0" {
		t.Errorf("apiId = %q, want the gateway handle", sub.APIID)
	}
	if token != testToken {
		t.Error("the gateway did not receive the portal's own token")
	}
	if sub.ApplicationID != "portal-app-1" || sub.Status != "ACTIVE" {
		t.Errorf("subscription = %+v", sub)
	}
	if sub.SubscriptionPlanID == "" {
		t.Error("no plan attached; the Gold plan should have been created")
	}
	record, ok := h.store.Subscription("portal-sub-1")
	if !ok || record.GatewaySubscriptionID != sub.ID || record.Token != testToken {
		t.Errorf("the mapping was not persisted correctly: %+v", record)
	}
}

// TestAPIResolutionToleratesDisplayNameFormatting covers why this service
// exists: the gateway's APIs came from API Manager, so its display name need
// not match the portal's character for character.
func TestAPIResolutionToleratesDisplayNameFormatting(t *testing.T) {
	for name, gatewayName := range map[string]string{
		"exact":          "Order API",
		"different case": "order api",
		"hyphenated":     "Order-API",
		"no separators":  "OrderAPI",
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeGateway()
			fake.addAPI(gatewayName, "v1.0", "order-api-v1.0")
			h := newHarness(t, fake, nil)
			if err := h.applyNow(t, createdEvent(t, "evt-1")); err != nil {
				t.Fatalf("apply() with gateway name %q = %v", gatewayName, err)
			}
			if sub, _ := fake.onlySub(t); sub.APIID != "order-api-v1.0" {
				t.Error("the subscription did not land on the expected API")
			}
		})
	}
}

func TestAPIResolutionHonoursAnOverride(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Completely Unrelated", "9.9", "legacy-orders")
	h := newHarness(t, fake, map[string]string{"cp-api-1": "legacy-orders"})

	if err := h.applyNow(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("apply() with an override = %v", err)
	}
	if sub, _ := fake.onlySub(t); sub.APIID != "legacy-orders" {
		t.Error("the override was not applied")
	}
}

// TestUnresolvableAPIFailsPermanently asserts an API the gateway does not have
// is not retried forever, and that the error says how to fix it.
func TestUnresolvableAPIFailsPermanently(t *testing.T) {
	h := newHarness(t, newFakeGateway(), nil)
	err := h.applyNow(t, createdEvent(t, "evt-1"))
	if !isPermanent(err) {
		t.Fatalf("apply() with no matching API = %v, want a permanent failure", err)
	}
	if !strings.Contains(err.Error(), "api_handles") {
		t.Errorf("the error does not tell an operator how to fix it: %v", err)
	}
}

func TestNonRestAPIKindIsRejectedPermanently(t *testing.T) {
	fake := newFakeGateway()
	h := newHarness(t, fake, nil)
	body := buildEvent(t, "evt-mcp", webhook.EventSubscriptionCreated, map[string]any{
		"subscription_id": "portal-sub-mcp",
		"api":             map[string]any{"ref_id": "cp-api-2", "name": "Tools", "version": "v1", "type": "Mcp"},
		"token":           encryptField(t, testToken),
	}, "token")
	if err := h.applyNow(t, body); !isPermanent(err) {
		t.Fatalf("apply() for an Mcp artifact = %v, want a permanent failure", err)
	}
	if fake.subCount() != 0 {
		t.Error("a subscription was created for an unsupported kind")
	}
}

func TestMissingTokenFailsPermanently(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)
	body := buildEvent(t, "evt-1", webhook.EventSubscriptionCreated, map[string]any{
		"subscription_id": "portal-sub-1", "api": apiRef(),
	})
	err := h.applyNow(t, body)
	if !isPermanent(err) {
		t.Fatalf("apply() with no token = %v, want a permanent failure", err)
	}
	if !strings.Contains(err.Error(), "config.toml") {
		t.Errorf("the error does not point at the secret mismatch: %v", err)
	}
}

func TestStatusChangeUpdatesInPlace(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)
	if err := h.applyNow(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("create = %v", err)
	}
	before, _ := fake.onlySub(t)

	body := buildEvent(t, "evt-2", webhook.EventSubscriptionUpdated, map[string]any{
		"subscription_id": "portal-sub-1", "status": "INACTIVE",
		"application_id": "portal-app-1", "api": apiRef(),
	})
	if err := h.applyNow(t, body); err != nil {
		t.Fatalf("apply(subscription.updated) = %v", err)
	}
	after, _ := fake.onlySub(t)
	if after.ID != before.ID {
		t.Error("the status change replaced the subscription instead of updating it")
	}
	if after.Status != "INACTIVE" {
		t.Errorf("status = %q, want INACTIVE", after.Status)
	}
}

func TestUnknownStatusDoesNotWidenAccess(t *testing.T) {
	if got := normalizeStatus("something-new"); got != gateway.StatusInactive {
		t.Fatalf("normalizeStatus(unknown) = %q, want INACTIVE", got)
	}
	if got := normalizeStatus("active"); got != gateway.StatusActive {
		t.Fatalf("normalizeStatus(lowercase) = %q, want ACTIVE", got)
	}
}

// TestPlanChangeRecreatesWithTheSameToken covers the constraint that the gateway
// has no plan-change operation.
func TestPlanChangeRecreatesWithTheSameToken(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)
	if err := h.applyNow(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("create = %v", err)
	}
	original, _ := fake.onlySub(t)

	body := buildEvent(t, "evt-2", webhook.EventSubscriptionPlanChanged, map[string]any{
		"subscription_id": "portal-sub-1", "status": "ACTIVE", "application_id": "portal-app-1",
		"subscription_plan": map[string]string{"ref_id": "portal-plan-platinum", "name": "Platinum"},
		"api":               apiRef(),
	})
	if err := h.applyNow(t, body); err != nil {
		t.Fatalf("apply(subscription.plan_changed) = %v", err)
	}

	updated, token := fake.onlySub(t)
	if token != testToken {
		t.Error("the recreated subscription lost the developer's existing token")
	}
	if updated.SubscriptionPlanID == original.SubscriptionPlanID {
		t.Error("the plan did not change")
	}
	if updated.Status != "ACTIVE" {
		t.Errorf("status = %q, want ACTIVE preserved", updated.Status)
	}
	if record, _ := h.store.Subscription("portal-sub-1"); record.GatewaySubscriptionID != updated.ID {
		t.Error("the store still points at the deleted subscription")
	}
}

func TestTokenRegenerationReplacesTheToken(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)
	if err := h.applyNow(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("create = %v", err)
	}

	const rotated = "sub-token-rotated-8c2e5a91-3b7d-4f16-a0e4"
	body := buildEvent(t, "evt-2", webhook.EventSubscriptionTokenRegenerated, map[string]any{
		"subscription_id": "portal-sub-1", "status": "ACTIVE", "application_id": "portal-app-1",
		"subscription_plan": map[string]string{"ref_id": "portal-plan-gold", "name": "Gold"},
		"api":               apiRef(), "token": encryptField(t, rotated),
	}, "token")
	if err := h.applyNow(t, body); err != nil {
		t.Fatalf("apply(subscription.token_regenerated) = %v", err)
	}
	if _, token := fake.onlySub(t); token != rotated {
		t.Error("the gateway still holds the old token")
	}
	if record, _ := h.store.Subscription("portal-sub-1"); record.Token != rotated {
		t.Error("the store did not take the rotated token")
	}
}

func TestSubscriptionDeletedRevokesAtTheGateway(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)
	if err := h.applyNow(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("create = %v", err)
	}
	body := buildEvent(t, "evt-2", webhook.EventSubscriptionDeleted, map[string]any{
		"subscription_id": "portal-sub-1", "application_id": "portal-app-1", "api": apiRef(),
	})
	if err := h.applyNow(t, body); err != nil {
		t.Fatalf("apply(subscription.deleted) = %v", err)
	}
	if fake.subCount() != 0 {
		t.Error("the gateway subscription was not deleted")
	}
	if _, ok := h.store.Subscription("portal-sub-1"); ok {
		t.Error("the store still maps a deleted subscription")
	}
}

func TestDeleteOfAnUnknownSubscriptionIsNotAnError(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)
	body := buildEvent(t, "evt-1", webhook.EventSubscriptionDeleted, map[string]any{
		"subscription_id": "never-seen", "application_id": "portal-app-9", "api": apiRef(),
	})
	if err := h.applyNow(t, body); err != nil {
		t.Fatalf("apply() for an unknown subscription = %v, want nil", err)
	}
}

func TestDuplicateCreateConvergesOnTheExistingSubscription(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)
	if err := h.applyNow(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("first create = %v", err)
	}
	original, _ := fake.onlySub(t)

	if err := h.applyNow(t, createdEvent(t, "evt-1-again")); err != nil {
		t.Fatalf("duplicate create = %v, want it to converge", err)
	}
	if fake.subCount() != 1 {
		t.Fatalf("the gateway holds %d subscriptions after a duplicate, want 1", fake.subCount())
	}
	if record, _ := h.store.Subscription("portal-sub-1"); record.GatewaySubscriptionID != original.ID {
		t.Error("the duplicate repointed the mapping")
	}
}

// TestPlanRejectedByTheAPIFallsBackToNoPlan asserts access still works when the
// gateway API's own plan allowlist excludes the portal's plan.
func TestPlanRejectedByTheAPIFallsBackToNoPlan(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0", "Bronze") // Gold not allowed
	h := newHarness(t, fake, nil)

	if err := h.applyNow(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("apply() = %v, want a fallback to no plan", err)
	}
	sub, token := fake.onlySub(t)
	if sub.SubscriptionPlanID != "" {
		t.Error("the subscription kept a plan the API does not allow")
	}
	if token != testToken {
		t.Error("the fallback lost the token")
	}
}

// An allowlist widened on the gateway must take effect on the next event, not
// once the cached copy expires. The cache has no invalidation signal for this —
// nothing on the portal side knows the gateway changed — so a plan change
// arriving inside APICacheTTL was silently applied without its plan, leaving
// the subscription unthrottled.
func TestWidenedAllowlistIsSeenBeforeTheCacheExpires(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0", "Gold") // Platinum not allowed yet
	h := newHarness(t, fake, nil)

	if err := h.applyNow(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("create = %v", err)
	}
	if sub, _ := fake.onlySub(t); sub.SubscriptionPlanID == "" {
		t.Fatal("the initial subscription lost its allowed plan")
	}

	// The operator widens the gateway API. No event announces it, and the
	// resolution cached by the create above is still well inside its TTL.
	fake.setAPIPlans("order-api-v1.0", "Gold", "Platinum")

	body := buildEvent(t, "evt-2", webhook.EventSubscriptionPlanChanged, map[string]any{
		"subscription_id": "portal-sub-1", "status": "ACTIVE", "application_id": "portal-app-1",
		"subscription_plan": map[string]string{"ref_id": "portal-plan-platinum", "name": "Platinum"},
		"api":               apiRef(),
	})
	if err := h.applyNow(t, body); err != nil {
		t.Fatalf("apply(subscription.plan_changed) = %v", err)
	}

	sub, _ := fake.onlySub(t)
	if sub.SubscriptionPlanID == "" {
		t.Fatal("the plan was dropped on a stale allowlist; the subscription is left unthrottled")
	}
	plan, ok := fake.plans[sub.SubscriptionPlanID]
	if !ok || !strings.EqualFold(plan.PlanName, "Platinum") {
		t.Errorf("subscription carries plan %q, want Platinum", sub.SubscriptionPlanID)
	}
}

// The confirming re-read must not turn the genuine fallback into a retry loop
// or a kept plan: a plan absent from a fresh allowlist is still dropped.
func TestPlanAbsentFromAFreshAllowlistIsStillDropped(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0", "Bronze") // Gold never allowed
	h := newHarness(t, fake, nil)

	if err := h.applyNow(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("apply() = %v, want a fallback to no plan", err)
	}
	if sub, _ := fake.onlySub(t); sub.SubscriptionPlanID != "" {
		t.Error("the subscription kept a plan no reading of the allowlist allows")
	}
}

func TestExistingGatewayPlanIsReused(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	fake.plans["existing-gold"] = &gateway.SubscriptionPlan{ID: "existing-gold", PlanName: "Gold", Status: "ACTIVE"}
	h := newHarness(t, fake, nil)

	if err := h.applyNow(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("apply() = %v", err)
	}
	if sub, _ := fake.onlySub(t); sub.SubscriptionPlanID != "existing-gold" {
		t.Errorf("subscriptionPlanId = %q, want the existing plan", sub.SubscriptionPlanID)
	}
	fake.mu.Lock()
	planCount := len(fake.plans)
	fake.mu.Unlock()
	if planCount != 1 {
		t.Errorf("the gateway holds %d plans, want just the pre-existing one", planCount)
	}
}

func TestAPIKeyLifecycle(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)

	const secret = "apip_a1b2c3d4e5f60718293a4b5c6d7e8f90112233445566778899aabbccddeeff00"
	generate := buildEvent(t, "evt-key-1", webhook.EventAPIKeyGenerated, map[string]any{
		"key_id": "portal-key-1", "handle": "my-production-key", "display_name": "My Production Key",
		"api": apiRef(), "key": encryptField(t, secret),
	}, "key")
	if err := h.applyNow(t, generate); err != nil {
		t.Fatalf("apply(apikey.generated) = %v", err)
	}
	sideMu.Lock()
	got := keyValueStore["order-api-v1.0/my-production-key"]
	sideMu.Unlock()
	if got != secret {
		t.Error("the gateway did not receive the portal's own key value")
	}

	const rotated = "apip_ff00112233445566778899aabbccddeeff0011223344556677889900aabbccdd"
	regenerate := buildEvent(t, "evt-key-2", webhook.EventAPIKeyRegenerated, map[string]any{
		"key_id": "portal-key-1", "handle": "my-production-key",
		"api": apiRef(), "key": encryptField(t, rotated),
	}, "key")
	if err := h.applyNow(t, regenerate); err != nil {
		t.Fatalf("apply(apikey.regenerated) = %v", err)
	}
	sideMu.Lock()
	got = keyValueStore["order-api-v1.0/my-production-key"]
	sideMu.Unlock()
	if got != rotated {
		t.Error("the gateway still holds the old key value")
	}

	revoke := buildEvent(t, "evt-key-3", webhook.EventAPIKeyRevoked, map[string]any{
		"key_id": "portal-key-1", "handle": "my-production-key", "api": apiRef(),
	})
	if err := h.applyNow(t, revoke); err != nil {
		t.Fatalf("apply(apikey.revoked) = %v", err)
	}
	fake.mu.Lock()
	_, stillThere := fake.keys["order-api-v1.0/my-production-key"]
	fake.mu.Unlock()
	if stillThere {
		t.Error("the revoked key is still at the gateway")
	}
	if _, ok := h.store.APIKeyByPortalID("portal-key-1"); ok {
		t.Error("the store still maps a revoked key")
	}
}

func TestRevokeOfAnUnknownKeyIsNotAnError(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)
	revoke := buildEvent(t, "evt-key-1", webhook.EventAPIKeyRevoked, map[string]any{
		"key_id": "never-seen", "handle": "ghost-key", "api": apiRef(),
	})
	if err := h.applyNow(t, revoke); err != nil {
		t.Fatalf("apply() for an unknown key = %v, want nil", err)
	}
}

// TestRegenerateCreatesTheKeyWhenGenerateWasMissed asserts recovery from the
// portal's exactly-once delivery losing an event.
func TestRegenerateCreatesTheKeyWhenGenerateWasMissed(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)

	const secret = "apip_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbe"
	body := buildEvent(t, "evt-key-1", webhook.EventAPIKeyRegenerated, map[string]any{
		"key_id": "portal-key-1", "handle": "orphan-key", "api": apiRef(), "key": encryptField(t, secret),
	}, "key")
	if err := h.applyNow(t, body); err != nil {
		t.Fatalf("apply() with no prior key = %v", err)
	}
	fake.mu.Lock()
	_, ok := fake.keys["order-api-v1.0/orphan-key"]
	fake.mu.Unlock()
	if !ok {
		t.Error("the mediator did not create the key a missed generate would have")
	}
}

// TestShortAPIKeyIsRejectedRatherThanAltered asserts a credential the gateway
// cannot accept is reported, never padded or truncated into shape.
func TestShortAPIKeyIsRejectedRatherThanAltered(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)
	body := buildEvent(t, "evt-key-1", webhook.EventAPIKeyGenerated, map[string]any{
		"key_id": "portal-key-1", "handle": "short-key", "api": apiRef(), "key": encryptField(t, "too-short"),
	}, "key")
	if err := h.applyNow(t, body); !isPermanent(err) {
		t.Fatalf("apply() with a short key = %v, want a permanent failure", err)
	}
	fake.mu.Lock()
	keyCount := len(fake.keys)
	fake.mu.Unlock()
	if keyCount != 0 {
		t.Error("a key the gateway would reject was sent anyway")
	}
}

func TestApplicationEventIsAcknowledgedWithoutAGatewayCall(t *testing.T) {
	h := newHarness(t, newFakeGateway(), nil)
	body := buildEvent(t, "evt-app-1", "application.created", map[string]any{
		"application_id": "portal-app-1", "display_name": "My App",
	})
	if err := h.applyNow(t, body); err != nil {
		t.Fatalf("apply(application.created) = %v, want nil", err)
	}
}

func TestUnknownEventTypeIsAcknowledged(t *testing.T) {
	h := newHarness(t, newFakeGateway(), nil)
	body := buildEvent(t, "evt-x", "something.brand_new", map[string]any{"whatever": true})
	if err := h.applyNow(t, body); err != nil {
		t.Fatalf("apply() for an unknown event type = %v, want nil", err)
	}
}

// TestTransientGatewayFailureIsRetriedThroughTheQueue exercises the full
// accept-queue-dispatch-retry loop against a gateway that is briefly
// unavailable — the case the durable queue exists for.
func TestTransientGatewayFailureIsRetriedThroughTheQueue(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	fake.failCreateTimes = 2
	h := newHarness(t, fake, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.med.Start(ctx)

	body := createdEvent(t, "evt-1")
	env, err := webhook.Decode(body)
	if err != nil {
		t.Fatalf("Decode() = %v", err)
	}
	if queued, err := h.med.Accept(env, body); err != nil || !queued {
		t.Fatalf("Accept() = %v, %v", queued, err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && fake.subCount() != 1 {
		time.Sleep(20 * time.Millisecond)
	}
	if fake.subCount() != 1 {
		ready, failed := h.store.PendingStats()
		t.Fatalf("the subscription was never created (queue: %d ready, %d failed)", ready, failed)
	}
	for time.Now().Before(deadline) {
		if ready, _ := h.store.PendingStats(); ready == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ready, failed := h.store.PendingStats(); ready != 0 || failed != 0 {
		t.Fatalf("the queue did not drain: %d ready, %d failed", ready, failed)
	}
	if !h.store.AlreadyProcessed("evt-1") {
		t.Error("the applied event was not recorded in the processed ledger")
	}
	if applied, permanent := h.med.Stats(); applied != 1 || permanent != 0 {
		t.Errorf("Stats() = %d applied, %d permanent; want 1, 0", applied, permanent)
	}
}

func TestDuplicateEventIDIsNotReapplied(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)

	body := createdEvent(t, "evt-1")
	env, _ := webhook.Decode(body)
	if err := h.store.MarkProcessed("evt-1"); err != nil {
		t.Fatalf("MarkProcessed() = %v", err)
	}
	queued, err := h.med.Accept(env, body)
	if err != nil {
		t.Fatalf("Accept() = %v", err)
	}
	if queued {
		t.Fatal("Accept() queued an event that was already applied")
	}
}

// TestPermanentFailureExhaustsImmediately asserts the retry budget is not spent
// on something no retry can fix.
func TestPermanentFailureExhaustsImmediately(t *testing.T) {
	h := newHarness(t, newFakeGateway(), nil) // no APIs — resolution can never succeed

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.med.Start(ctx)

	body := createdEvent(t, "evt-1")
	env, _ := webhook.Decode(body)
	if _, err := h.med.Accept(env, body); err != nil {
		t.Fatalf("Accept() = %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, failed := h.store.PendingStats(); failed == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	records := h.store.FailedPending(10)
	if len(records) != 1 {
		t.Fatalf("FailedPending() = %d records, want 1", len(records))
	}
	if records[0].Attempts != 1 {
		t.Errorf("a permanent failure consumed %d attempts, want exactly 1", records[0].Attempts)
	}
}

// TestEventsForOneSubscriptionShareALane asserts the ordering guarantee: a
// create and the delete that follows it can never be applied out of order.
func TestEventsForOneSubscriptionShareALane(t *testing.T) {
	created := createdEvent(t, "evt-1")
	deleted := buildEvent(t, "evt-2", webhook.EventSubscriptionDeleted, map[string]any{
		"subscription_id": "portal-sub-1", "api": apiRef(),
	})
	createdEnv, _ := webhook.Decode(created)
	deletedEnv, _ := webhook.Decode(deleted)

	createdKey := orderingKey(createdEnv, created)
	if createdKey != "sub:portal-sub-1" {
		t.Fatalf("orderingKey() = %q, want it keyed on the subscription", createdKey)
	}
	deletedKey := orderingKey(deletedEnv, deleted)
	if createdKey != deletedKey {
		t.Fatalf("ordering keys differ: %q and %q", createdKey, deletedKey)
	}
	for lanes := 1; lanes <= 8; lanes++ {
		if laneFor(createdKey, lanes) != laneFor(deletedKey, lanes) {
			t.Fatalf("with %d lanes the two events were routed apart", lanes)
		}
	}
}

func TestOrderingKeyFallsBackToTheEventID(t *testing.T) {
	body := buildEvent(t, "evt-1", "x.y", map[string]any{"nothing": "here"})
	env, _ := webhook.Decode(body)
	if got := orderingKey(env, body); got != "event:evt-1" {
		t.Fatalf("orderingKey() = %q, want the event-id fallback", got)
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	for attempt := 1; attempt <= 12; attempt++ {
		delay := backoff(attempt)
		if delay <= 0 || delay > RetryMaxDelay {
			t.Fatalf("backoff(%d) = %v, outside (0, %v]", attempt, delay, RetryMaxDelay)
		}
	}
}

func TestJitteredIntervalStaysInRange(t *testing.T) {
	sawVariation := false
	first := jitteredInterval()
	for i := 0; i < 200; i++ {
		got := jitteredInterval()
		if got < ScanInterval || got >= ScanInterval*3/2 {
			t.Fatalf("jitteredInterval() = %v, outside [%v, %v)", got, ScanInterval, ScanInterval*3/2)
		}
		if got != first {
			sawVariation = true
		}
	}
	if !sawVariation {
		t.Error("jitteredInterval() returned a constant; instances would not desynchronize")
	}
}

func TestSlugifyAndNormalize(t *testing.T) {
	for input, want := range map[string]string{
		"My Production Key": "my-production-key",
		"Order API":         "order-api",
		"UPPER_snake.case":  "upper-snake-case",
		"!!!":               "",
	} {
		if got := slugify(input); got != want {
			t.Errorf("slugify(%q) = %q, want %q", input, got, want)
		}
	}
	if normalizeName("Order API") != normalizeName("order-api") {
		t.Error("normalizeName() does not fold punctuation and case")
	}
	if normalizeVersion("v1.0") != "1.0" {
		t.Error("normalizeVersion() does not fold the v prefix")
	}
}

func TestPlanEnabledForAPI(t *testing.T) {
	if !planEnabledForAPI(resolvedAPI{}, "Gold") {
		t.Error("an API with no allowlist should accept any plan")
	}
	if !planEnabledForAPI(resolvedAPI{planNames: []string{"gold"}}, "Gold") {
		t.Error("the allowlist comparison should be case-insensitive")
	}
	if planEnabledForAPI(resolvedAPI{planNames: []string{"Bronze"}}, "Gold") {
		t.Error("a plan outside the allowlist should not be accepted")
	}
}
