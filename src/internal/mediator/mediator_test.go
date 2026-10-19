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
	"strings"
	"sync"
	"testing"
	"time"

	"subscription-mediator/internal/gateway"
	"subscription-mediator/internal/webhook"
)

const (
	testSecret = "a-webhook-subscriber-secret-at-least-32-chars"
	testToken  = "sub-token-3f9a1c7e-5b2d-4e81-9a03-6c4f7b1e8d25"
)

// fakeGateway stands in for the Gateway Controller management REST API, with
// the subscription changes the mediator relies on: a caller-supplied
// externalRefId that becomes the subscription ID, and a partial PUT that takes
// the plan and the token. The tests assert on the gateway's end state rather than on the
// mediator's own calls.
type fakeGateway struct {
	mu     sync.Mutex
	apis   []gateway.RestAPI
	plans  map[string]*gateway.SubscriptionPlan
	subs   map[string]*gateway.Subscription
	tokens map[string]string          // subscription ID -> token; the gateway never returns it
	keys   map[string]*gateway.APIKey // "<apiHandle>/<keyName>"
	values map[string]string          // "<apiHandle>/<keyName>" -> key value

	// failCreateTimes makes the next N subscription creates fail with a 503.
	failCreateTimes int
	nextID          int
}

func newFakeGateway() *fakeGateway {
	return &fakeGateway{
		plans:  map[string]*gateway.SubscriptionPlan{},
		subs:   map[string]*gateway.Subscription{},
		tokens: map[string]string{},
		keys:   map[string]*gateway.APIKey{},
		values: map[string]string{},
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

// planAllowedLocked mirrors the gateway rejecting a plan the API's allowlist
// excludes.
func (f *fakeGateway) planAllowedLocked(apiID, planID string) bool {
	for _, api := range f.apis {
		if api.Handle() != apiID || len(api.Spec.SubscriptionPlans) == 0 {
			continue
		}
		plan, ok := f.plans[planID]
		if !ok {
			return false
		}
		for _, name := range api.Spec.SubscriptionPlans {
			if strings.EqualFold(plan.PlanName, name) {
				return true
			}
		}
		return false
	}
	return true
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
		if req.SubscriptionPlanID != "" && !f.planAllowedLocked(req.APIID, req.SubscriptionPlanID) {
			writeJSON(w, 400, map[string]string{"message": "Subscription plan is not enabled for this API"})
			return
		}
		// Mirror the gateway's uniqueness: one token per API, and the
		// externalRefId is the primary key.
		for id, sub := range f.subs {
			if (sub.APIID == req.APIID && f.tokens[id] == req.SubscriptionToken) ||
				(req.ExternalRefID != "" && id == req.ExternalRefID) {
				writeJSON(w, 409, map[string]string{"message": "Subscription already exists"})
				return
			}
		}
		subID := req.ExternalRefID
		if subID == "" {
			subID = f.id("sub")
		}
		sub := &gateway.Subscription{
			ID: subID, ExternalRefID: req.ExternalRefID, APIID: req.APIID, ApplicationID: req.ApplicationID,
			SubscriptionPlanID: req.SubscriptionPlanID, Status: req.Status,
		}
		f.subs[sub.ID] = sub
		f.tokens[sub.ID] = req.SubscriptionToken
		writeJSON(w, 201, sub)
	})

	mux.HandleFunc("GET /api/management/v1/subscriptions/{subscriptionId}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		sub, ok := f.subs[r.PathValue("subscriptionId")]
		if !ok {
			writeJSON(w, 404, map[string]string{"message": "Subscription not found"})
			return
		}
		writeJSON(w, 200, sub)
	})

	mux.HandleFunc("PUT /api/management/v1/subscriptions/{subscriptionId}", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]json.RawMessage
		decode(t, r, &req)
		f.mu.Lock()
		defer f.mu.Unlock()
		sub, ok := f.subs[r.PathValue("subscriptionId")]
		if !ok {
			writeJSON(w, 404, map[string]string{"message": "Subscription not found"})
			return
		}
		for field := range req {
			if field != "status" && field != "subscriptionPlanId" && field != "subscriptionToken" {
				writeJSON(w, 400, map[string]string{"message": "unknown field " + field})
				return
			}
		}
		if raw, ok := req["subscriptionPlanId"]; ok {
			var planID *string
			_ = json.Unmarshal(raw, &planID)
			switch {
			case planID == nil:
				sub.SubscriptionPlanID = ""
			case !f.planAllowedLocked(sub.APIID, *planID):
				writeJSON(w, 400, map[string]string{"message": "Subscription plan is not enabled for this API"})
				return
			default:
				sub.SubscriptionPlanID = *planID
			}
		}
		if raw, ok := req["status"]; ok {
			_ = json.Unmarshal(raw, &sub.Status)
		}
		if raw, ok := req["subscriptionToken"]; ok {
			var token string
			_ = json.Unmarshal(raw, &token)
			f.tokens[sub.ID] = token
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
		delete(f.tokens, id)
		w.WriteHeader(204)
	})

	mux.HandleFunc("GET /api/management/v1/rest-apis/{id}/api-keys", func(w http.ResponseWriter, r *http.Request) {
		apiHandle := r.PathValue("id")
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.hasAPILocked(apiHandle) {
			writeJSON(w, 404, map[string]string{"message": "RestAPI not found"})
			return
		}
		out := []gateway.APIKey{}
		for _, key := range f.keys {
			if key.APIID == apiHandle {
				out = append(out, *key)
			}
		}
		writeJSON(w, 200, map[string]any{"apiKeys": out, "totalCount": len(out)})
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
		// A revoked key keeps its name, so the name stays taken.
		if _, exists := f.keys[mapKey]; exists {
			writeJSON(w, 409, map[string]string{"message": "API key already exists"})
			return
		}
		key := &gateway.APIKey{Name: req.Name, APIID: apiHandle, Status: "active", ExternalRefID: req.ExternalRefID}
		f.keys[mapKey] = key
		f.values[mapKey] = req.APIKey
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
		f.values[mapKey] = req.APIKey
		// Like gateway 1.2.0: an update rewrites external_ref_id from a value it
		// never sets, so the key loses its externalRefId.
		key.ExternalRefID = ""
		writeJSON(w, 200, map[string]any{"apiKey": key})
	})

	mux.HandleFunc("DELETE /api/management/v1/rest-apis/{id}/api-keys/{apiKeyName}", func(w http.ResponseWriter, r *http.Request) {
		mapKey := r.PathValue("id") + "/" + r.PathValue("apiKeyName")
		f.mu.Lock()
		defer f.mu.Unlock()
		key, ok := f.keys[mapKey]
		if !ok {
			writeJSON(w, 404, map[string]string{"message": "API key not found"})
			return
		}
		key.Status = gateway.KeyStatusRevoked
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
		return *sub, f.tokens[sub.ID]
	}
	return gateway.Subscription{}, ""
}

func (f *fakeGateway) key(apiHandle, name string) (gateway.APIKey, string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key, ok := f.keys[apiHandle+"/"+name]
	if !ok {
		return gateway.APIKey{}, "", false
	}
	return *key, f.values[apiHandle+"/"+name], true
}

func (f *fakeGateway) keyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.keys)
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
		t.Errorf("decoding %q: %v", body, err)
	}
}

// --- harness ---------------------------------------------------------------

type harness struct {
	med  *Mediator
	fake *fakeGateway
}

func newHarness(t *testing.T, fake *fakeGateway, overrides map[string]string) *harness {
	t.Helper()
	baseURL := fake.start(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	codec, err := webhook.NewCodec(testSecret)
	if err != nil {
		t.Fatalf("webhook.NewCodec() = %v", err)
	}
	if overrides == nil {
		overrides = map[string]string{}
	}
	client, err := gateway.New(baseURL, "foo", "bar", "")
	if err != nil {
		t.Fatalf("gateway.New() = %v", err)
	}
	return &harness{med: New(client, codec, overrides, logger), fake: fake}
}

// handle applies one event the way the HTTP layer does.
func (h *harness) handle(t *testing.T, body []byte) error {
	t.Helper()
	env, err := webhook.Decode(body)
	if err != nil {
		t.Fatalf("webhook.Decode() = %v", err)
	}
	return h.med.Handle(context.Background(), env, body)
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

// planChangedEvent is a plan change as the portal sends it: no token.
func planChangedEvent(t *testing.T, id, subscriptionID, planName string) []byte {
	return buildEvent(t, id, webhook.EventSubscriptionPlanChanged, map[string]any{
		"subscription_id": subscriptionID, "status": "ACTIVE",
		"subscription_plan": map[string]string{"ref_id": "portal-plan-" + strings.ToLower(planName), "name": planName},
		"api":               apiRef(),
	})
}

func keyEvent(t *testing.T, id, eventType, keyID, handle, value string) []byte {
	data := map[string]any{"key_id": keyID, "handle": handle, "api": apiRef()}
	if value == "" {
		return buildEvent(t, id, eventType, data)
	}
	data["key"] = encryptField(t, value)
	return buildEvent(t, id, eventType, data, "key")
}

// --- subscriptions ---------------------------------------------------------

// TestSubscriptionCreatedBecomesAGatewaySubscription is the core translation: a
// portal event resolved onto an APIM-deployed gateway API by display name and
// version, with the portal's own token injected and the portal's subscription
// ID as the reference every later event looks it up by.
func TestSubscriptionCreatedBecomesAGatewaySubscription(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)

	if err := h.handle(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("Handle(subscription.created) = %v", err)
	}

	sub, token := fake.onlySub(t)
	if sub.ID != "portal-sub-1" {
		t.Errorf("subscription ID = %q, want the portal subscription ID", sub.ID)
	}
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
			if err := h.handle(t, createdEvent(t, "evt-1")); err != nil {
				t.Fatalf("Handle() with gateway name %q = %v", gatewayName, err)
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

	if err := h.handle(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("Handle() with an override = %v", err)
	}
	if sub, _ := fake.onlySub(t); sub.APIID != "legacy-orders" {
		t.Error("the override was not applied")
	}
}

// TestUnresolvableAPIFailsPermanently asserts an API the gateway does not have
// is reported as unfixable by a retry, and that the error says how to fix it.
func TestUnresolvableAPIFailsPermanently(t *testing.T) {
	h := newHarness(t, newFakeGateway(), nil)
	err := h.handle(t, createdEvent(t, "evt-1"))
	if !IsPermanent(err) {
		t.Fatalf("Handle() with no matching API = %v, want a permanent failure", err)
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
	if err := h.handle(t, body); !IsPermanent(err) {
		t.Fatalf("Handle() for an Mcp artifact = %v, want a permanent failure", err)
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
	err := h.handle(t, body)
	if !IsPermanent(err) {
		t.Fatalf("Handle() with no token = %v, want a permanent failure", err)
	}
	if !strings.Contains(err.Error(), "config.toml") {
		t.Errorf("the error does not point at the secret mismatch: %v", err)
	}
}

func TestStatusChangeUpdatesInPlace(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)
	if err := h.handle(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("create = %v", err)
	}
	before, _ := fake.onlySub(t)

	body := buildEvent(t, "evt-2", webhook.EventSubscriptionUpdated, map[string]any{
		"subscription_id": "portal-sub-1", "status": "INACTIVE", "api": apiRef(),
	})
	if err := h.handle(t, body); err != nil {
		t.Fatalf("Handle(subscription.updated) = %v", err)
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

// TestPlanChangeUpdatesInPlaceWithoutAToken covers the reason no token is kept:
// the plan changes on the existing subscription, which keeps its token.
func TestPlanChangeUpdatesInPlaceWithoutAToken(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)
	if err := h.handle(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("create = %v", err)
	}
	original, _ := fake.onlySub(t)

	if err := h.handle(t, planChangedEvent(t, "evt-2", "portal-sub-1", "Platinum")); err != nil {
		t.Fatalf("Handle(subscription.plan_changed) = %v", err)
	}

	updated, token := fake.onlySub(t)
	if updated.ID != original.ID {
		t.Error("the plan change replaced the subscription instead of updating it")
	}
	if token != testToken {
		t.Error("the plan change lost the developer's existing token")
	}
	plan, ok := fake.plans[updated.SubscriptionPlanID]
	if !ok || plan.PlanName != "Platinum" {
		t.Errorf("subscription carries plan %q, want Platinum", updated.SubscriptionPlanID)
	}
	if updated.Status != "ACTIVE" {
		t.Errorf("status = %q, want ACTIVE preserved", updated.Status)
	}
}

// TestPlanChangeToADisallowedPlanClearsIt asserts access keeps working on
// default limits rather than the event failing.
func TestPlanChangeToADisallowedPlanClearsIt(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0", "Gold") // Platinum never allowed
	h := newHarness(t, fake, nil)
	if err := h.handle(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("create = %v", err)
	}
	if sub, _ := fake.onlySub(t); sub.SubscriptionPlanID == "" {
		t.Fatal("the initial subscription lost its allowed plan")
	}

	if err := h.handle(t, planChangedEvent(t, "evt-2", "portal-sub-1", "Platinum")); err != nil {
		t.Fatalf("Handle(subscription.plan_changed) = %v", err)
	}
	if sub, _ := fake.onlySub(t); sub.SubscriptionPlanID != "" {
		t.Errorf("subscription kept plan %q; a plan the API does not allow should be cleared", sub.SubscriptionPlanID)
	}
}

// An allowlist widened on the gateway must take effect on the next event, not
// once the cached copy expires. The cache has no invalidation signal for this —
// nothing on the portal side knows the gateway changed — so a plan change
// arriving inside APICacheTTL would otherwise lose its plan.
func TestWidenedAllowlistIsSeenBeforeTheCacheExpires(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0", "Gold") // Platinum not allowed yet
	h := newHarness(t, fake, nil)
	if err := h.handle(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("create = %v", err)
	}

	// The operator widens the gateway API. No event announces it, and the
	// resolution cached by the create above is still well inside its TTL.
	fake.setAPIPlans("order-api-v1.0", "Gold", "Platinum")

	if err := h.handle(t, planChangedEvent(t, "evt-2", "portal-sub-1", "Platinum")); err != nil {
		t.Fatalf("Handle(subscription.plan_changed) = %v", err)
	}
	sub, _ := fake.onlySub(t)
	plan, ok := fake.plans[sub.SubscriptionPlanID]
	if !ok || !strings.EqualFold(plan.PlanName, "Platinum") {
		t.Errorf("subscription carries plan %q, want Platinum", sub.SubscriptionPlanID)
	}
}

func TestTokenRegenerationReplacesTheTokenInPlace(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)
	if err := h.handle(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("create = %v", err)
	}
	original, _ := fake.onlySub(t)

	const rotated = "sub-token-rotated-8c2e5a91-3b7d-4f16-a0e4"
	body := buildEvent(t, "evt-2", webhook.EventSubscriptionTokenRegenerated, map[string]any{
		"subscription_id": "portal-sub-1", "status": "ACTIVE",
		"subscription_plan": map[string]string{"ref_id": "portal-plan-gold", "name": "Gold"},
		"api":               apiRef(), "token": encryptField(t, rotated),
	}, "token")
	if err := h.handle(t, body); err != nil {
		t.Fatalf("Handle(subscription.token_regenerated) = %v", err)
	}
	updated, token := fake.onlySub(t)
	if updated.ID != original.ID {
		t.Error("the rotation replaced the subscription instead of updating it")
	}
	if token != rotated {
		t.Error("the gateway still holds the old token")
	}
}

// TestTokenRegenerationCreatesAMissingSubscription asserts recovery from a lost
// subscription.created: the rotation event carries everything a create needs.
func TestTokenRegenerationCreatesAMissingSubscription(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)

	const rotated = "sub-token-rotated-8c2e5a91-3b7d-4f16-a0e4"
	body := buildEvent(t, "evt-1", webhook.EventSubscriptionTokenRegenerated, map[string]any{
		"subscription_id": "portal-sub-1", "status": "ACTIVE",
		"subscription_plan": map[string]string{"ref_id": "portal-plan-gold", "name": "Gold"},
		"api":               apiRef(), "token": encryptField(t, rotated),
	}, "token")
	if err := h.handle(t, body); err != nil {
		t.Fatalf("Handle(subscription.token_regenerated) with no subscription = %v", err)
	}
	sub, token := fake.onlySub(t)
	if sub.ID != "portal-sub-1" || token != rotated {
		t.Errorf("created %+v with token %q, want the portal subscription with the rotated token", sub, token)
	}
}

// TestChangeToAnUnknownSubscriptionFailsPermanently asserts an update the
// gateway has no subscription for is reported, with what to do about it.
func TestChangeToAnUnknownSubscriptionFailsPermanently(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)

	for name, body := range map[string][]byte{
		"status change": buildEvent(t, "evt-1", webhook.EventSubscriptionUpdated, map[string]any{
			"subscription_id": "never-seen", "status": "INACTIVE", "api": apiRef(),
		}),
		"plan change": planChangedEvent(t, "evt-2", "never-seen", "Gold"),
	} {
		err := h.handle(t, body)
		if !IsPermanent(err) {
			t.Fatalf("%s for an unknown subscription = %v, want a permanent failure", name, err)
		}
		if !strings.Contains(err.Error(), "regenerate") {
			t.Errorf("%s: the error does not say how to recover: %v", name, err)
		}
	}
}

func TestSubscriptionDeletedRevokesAtTheGateway(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)
	if err := h.handle(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("create = %v", err)
	}
	body := buildEvent(t, "evt-2", webhook.EventSubscriptionDeleted, map[string]any{
		"subscription_id": "portal-sub-1", "api": apiRef(),
	})
	if err := h.handle(t, body); err != nil {
		t.Fatalf("Handle(subscription.deleted) = %v", err)
	}
	if fake.subCount() != 0 {
		t.Error("the gateway subscription was not deleted")
	}
}

// TestDeleteFindsTheRightSubscriptionAmongMany is the case a lookup by API and
// application could not decide: several subscriptions on one API, none with an
// application.
func TestDeleteFindsTheRightSubscriptionAmongMany(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)
	for i, subscriber := range []string{"alice", "bob"} {
		body := buildEvent(t, fmt.Sprintf("evt-%d", i), webhook.EventSubscriptionCreated, map[string]any{
			"subscription_id": "portal-sub-" + subscriber, "status": "ACTIVE", "api": apiRef(),
			"token": encryptField(t, testToken+"-"+subscriber),
		}, "token")
		if err := h.handle(t, body); err != nil {
			t.Fatalf("create for %s = %v", subscriber, err)
		}
	}

	body := buildEvent(t, "evt-del", webhook.EventSubscriptionDeleted, map[string]any{
		"subscription_id": "portal-sub-alice", "api": apiRef(),
	})
	if err := h.handle(t, body); err != nil {
		t.Fatalf("Handle(subscription.deleted) = %v", err)
	}
	remaining, _ := fake.onlySub(t)
	if remaining.ID != "portal-sub-bob" {
		t.Errorf("the remaining subscription is %q, want bob's", remaining.ID)
	}
}

func TestDeleteOfAnUnknownSubscriptionIsNotAnError(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)
	body := buildEvent(t, "evt-1", webhook.EventSubscriptionDeleted, map[string]any{
		"subscription_id": "never-seen", "api": apiRef(),
	})
	if err := h.handle(t, body); err != nil {
		t.Fatalf("Handle() for an unknown subscription = %v, want nil", err)
	}
}

func TestDuplicateCreateConvergesOnTheExistingSubscription(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)
	if err := h.handle(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("first create = %v", err)
	}
	original, _ := fake.onlySub(t)

	if err := h.handle(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("duplicate create = %v, want it to converge", err)
	}
	if again, _ := fake.onlySub(t); again.ID != original.ID {
		t.Error("the duplicate replaced the subscription")
	}
}

// TestConflictWithSomebodyElsesSubscriptionFailsPermanently asserts a 409 is
// only treated as a duplicate when the gateway holds a subscription under this
// event's subscription ID.
func TestConflictWithSomebodyElsesSubscriptionFailsPermanently(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	fake.subs["sub-other"] = &gateway.Subscription{ID: "sub-other", ExternalRefID: "sub-other", APIID: "order-api-v1.0"}
	fake.tokens["sub-other"] = testToken
	h := newHarness(t, fake, nil)

	if err := h.handle(t, createdEvent(t, "evt-1")); !IsPermanent(err) {
		t.Fatalf("Handle() into a conflicting subscription = %v, want a permanent failure", err)
	}
}

// TestPlanRejectedByTheAPIFallsBackToNoPlan asserts access still works when the
// gateway API's own plan allowlist excludes the portal's plan.
func TestPlanRejectedByTheAPIFallsBackToNoPlan(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0", "Bronze") // Gold not allowed
	h := newHarness(t, fake, nil)

	if err := h.handle(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("Handle() = %v, want a fallback to no plan", err)
	}
	sub, token := fake.onlySub(t)
	if sub.SubscriptionPlanID != "" {
		t.Error("the subscription kept a plan the API does not allow")
	}
	if token != testToken {
		t.Error("the fallback lost the token")
	}
}

func TestExistingGatewayPlanIsReused(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	fake.plans["existing-gold"] = &gateway.SubscriptionPlan{ID: "existing-gold", PlanName: "Gold", Status: "ACTIVE"}
	h := newHarness(t, fake, nil)

	if err := h.handle(t, createdEvent(t, "evt-1")); err != nil {
		t.Fatalf("Handle() = %v", err)
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

// TestTransientGatewayFailureIsNotPermanent asserts a gateway-side failure is
// reported as one, so the HTTP layer can tell it apart from a bad event.
func TestTransientGatewayFailureIsNotPermanent(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	fake.failCreateTimes = 1
	h := newHarness(t, fake, nil)

	err := h.handle(t, createdEvent(t, "evt-1"))
	if err == nil || IsPermanent(err) {
		t.Fatalf("Handle() against a failing gateway = %v, want a transient error", err)
	}
	if applied, failed := h.med.Stats(); applied != 0 || failed != 1 {
		t.Errorf("Stats() = %d applied, %d failed; want 0, 1", applied, failed)
	}

	// Repeating the action in the portal sends the event again, and it applies.
	if err := h.handle(t, createdEvent(t, "evt-2")); err != nil {
		t.Fatalf("Handle() once the gateway recovered = %v", err)
	}
	if applied, failed := h.med.Stats(); applied != 1 || failed != 1 {
		t.Errorf("Stats() = %d applied, %d failed; want 1, 1", applied, failed)
	}
}

// --- API keys --------------------------------------------------------------

// TestAPIKeyLifecycle runs generate, regenerate and revoke. The fake gateway,
// like the real one, drops a key's externalRefId on update, so the revoke only
// lands if the key is found by its name.
func TestAPIKeyLifecycle(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)

	const secret = "apip_a1b2c3d4e5f60718293a4b5c6d7e8f90112233445566778899aabbccddeeff00"
	if err := h.handle(t, keyEvent(t, "evt-1", webhook.EventAPIKeyGenerated, "portal-key-1", "my-production-key", secret)); err != nil {
		t.Fatalf("Handle(apikey.generated) = %v", err)
	}
	key, value, ok := fake.key("order-api-v1.0", keyNameFor("portal-key-1"))
	if !ok || value != secret {
		t.Fatal("the gateway did not receive the portal's own key value")
	}
	if key.ExternalRefID != "portal-key-1" {
		t.Errorf("externalRefId = %q, want the portal key ID", key.ExternalRefID)
	}

	const rotated = "apip_ff00112233445566778899aabbccddeeff0011223344556677889900aabbccdd"
	if err := h.handle(t, keyEvent(t, "evt-2", webhook.EventAPIKeyRegenerated, "portal-key-1", "my-production-key", rotated)); err != nil {
		t.Fatalf("Handle(apikey.regenerated) = %v", err)
	}
	if _, value, _ := fake.key("order-api-v1.0", keyNameFor("portal-key-1")); value != rotated {
		t.Error("the gateway still holds the old key value")
	}

	if err := h.handle(t, keyEvent(t, "evt-3", webhook.EventAPIKeyRevoked, "portal-key-1", "my-production-key", "")); err != nil {
		t.Fatalf("Handle(apikey.revoked) = %v", err)
	}
	if key, _, _ := fake.key("order-api-v1.0", keyNameFor("portal-key-1")); key.Status != gateway.KeyStatusRevoked {
		t.Errorf("status = %q, want the key revoked", key.Status)
	}
}

// TestCollidingKeyHandlesAreToldApart: two handles that slugify alike still get
// distinct gateway names, since names come from the portal key ID, and revoking
// one leaves the other alone.
func TestCollidingKeyHandlesAreToldApart(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)

	const aliceKey = "apip_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const bobKey = "apip_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := h.handle(t, keyEvent(t, "evt-1", webhook.EventAPIKeyGenerated, "key-alice-0001", "My Key", aliceKey)); err != nil {
		t.Fatalf("generate alice = %v", err)
	}
	if err := h.handle(t, keyEvent(t, "evt-2", webhook.EventAPIKeyGenerated, "key-bob-0002", "my_key", bobKey)); err != nil {
		t.Fatalf("generate bob = %v", err)
	}
	if fake.keyCount() != 2 {
		t.Fatalf("the gateway holds %d keys, want 2", fake.keyCount())
	}
	if _, value, _ := fake.key("order-api-v1.0", keyNameFor("key-alice-0001")); value != aliceKey {
		t.Error("the second key overwrote the first")
	}

	if err := h.handle(t, keyEvent(t, "evt-3", webhook.EventAPIKeyRevoked, "key-bob-0002", "my_key", "")); err != nil {
		t.Fatalf("revoke bob = %v", err)
	}
	if alice, _, _ := fake.key("order-api-v1.0", keyNameFor("key-alice-0001")); alice.Status == gateway.KeyStatusRevoked {
		t.Fatal("revoking bob's key revoked alice's")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, key := range fake.keys {
		if key.ExternalRefID == "key-bob-0002" && key.Status != gateway.KeyStatusRevoked {
			t.Error("bob's key is still live")
		}
	}
}

func TestDuplicateGenerateKeepsOneKey(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)

	const secret = "apip_a1b2c3d4e5f60718293a4b5c6d7e8f90112233445566778899aabbccddeeff00"
	for i := 0; i < 2; i++ {
		if err := h.handle(t, keyEvent(t, "evt-1", webhook.EventAPIKeyGenerated, "portal-key-1", "my-key", secret)); err != nil {
			t.Fatalf("generate #%d = %v", i+1, err)
		}
	}
	if fake.keyCount() != 1 {
		t.Errorf("the gateway holds %d keys after a duplicate generate, want 1", fake.keyCount())
	}
}

// TestNewKeyReusingARevokedHandleGetsItsOwnName: a new portal key with the
// handle of a revoked one gets its own gateway key, and the revoked one stays
// revoked.
func TestNewKeyReusingARevokedHandleGetsItsOwnName(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)

	const first = "apip_1111111111111111111111111111111111111111111111111111111111111111"
	const second = "apip_2222222222222222222222222222222222222222222222222222222222222222"
	steps := [][]byte{
		keyEvent(t, "evt-1", webhook.EventAPIKeyGenerated, "portal-key-1", "my-key", first),
		keyEvent(t, "evt-2", webhook.EventAPIKeyRevoked, "portal-key-1", "my-key", ""),
		keyEvent(t, "evt-3", webhook.EventAPIKeyGenerated, "portal-key-2", "my-key", second),
	}
	for i, body := range steps {
		if err := h.handle(t, body); err != nil {
			t.Fatalf("step %d = %v", i+1, err)
		}
	}
	if fake.keyCount() != 2 {
		t.Fatalf("the gateway holds %d keys, want the revoked one and the new one", fake.keyCount())
	}
	if old, _, _ := fake.key("order-api-v1.0", keyNameFor("portal-key-1")); old.Status != gateway.KeyStatusRevoked {
		t.Error("the new key revived the revoked one")
	}
}

// TestRevokedKeyIsNotRecreated: a generate for a portal key the gateway holds
// revoked (a late duplicate delivery) must not bring the credential back.
func TestRevokedKeyIsNotRecreated(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)

	const secret = "apip_3333333333333333333333333333333333333333333333333333333333333333"
	for i, body := range [][]byte{
		keyEvent(t, "evt-1", webhook.EventAPIKeyGenerated, "portal-key-1", "my-key", secret),
		keyEvent(t, "evt-2", webhook.EventAPIKeyRevoked, "portal-key-1", "my-key", ""),
	} {
		if err := h.handle(t, body); err != nil {
			t.Fatalf("step %d = %v", i+1, err)
		}
	}
	if err := h.handle(t, keyEvent(t, "evt-3", webhook.EventAPIKeyGenerated, "portal-key-1", "my-key", secret)); !IsPermanent(err) {
		t.Fatalf("generate of a revoked key = %v, want a permanent failure", err)
	}
	if key, _, _ := fake.key("order-api-v1.0", keyNameFor("portal-key-1")); key.Status != gateway.KeyStatusRevoked {
		t.Error("the revoked key was brought back")
	}
}

func TestRevokeOfAnUnknownKeyIsNotAnError(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)
	if err := h.handle(t, keyEvent(t, "evt-1", webhook.EventAPIKeyRevoked, "never-seen", "ghost-key", "")); err != nil {
		t.Fatalf("Handle() for an unknown key = %v, want nil", err)
	}
}

// TestRegenerateCreatesTheKeyWhenGenerateWasMissed asserts recovery from the
// portal's exactly-once delivery losing an event.
func TestRegenerateCreatesTheKeyWhenGenerateWasMissed(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)

	const secret = "apip_deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbe"
	if err := h.handle(t, keyEvent(t, "evt-1", webhook.EventAPIKeyRegenerated, "portal-key-1", "orphan-key", secret)); err != nil {
		t.Fatalf("Handle() with no prior key = %v", err)
	}
	if _, value, ok := fake.key("order-api-v1.0", keyNameFor("portal-key-1")); !ok || value != secret {
		t.Error("the mediator did not create the key a missed generate would have")
	}
}

// TestShortAPIKeyIsRejectedRatherThanAltered asserts a credential the gateway
// cannot accept is reported, never padded or truncated into shape.
func TestShortAPIKeyIsRejectedRatherThanAltered(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)
	body := keyEvent(t, "evt-1", webhook.EventAPIKeyGenerated, "portal-key-1", "short-key", "too-short")
	if err := h.handle(t, body); !IsPermanent(err) {
		t.Fatalf("Handle() with a short key = %v, want a permanent failure", err)
	}
	if fake.keyCount() != 0 {
		t.Error("a key the gateway would reject was sent anyway")
	}
}

// TestUnreadableExpiryIsRejectedRatherThanDropped: an expires_at the mediator
// cannot parse must not turn into a key that never expires.
func TestUnreadableExpiryIsRejectedRatherThanDropped(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)

	const secret = "apip_a1b2c3d4e5f60718293a4b5c6d7e8f90112233445566778899aabbccddeeff00"
	data := map[string]any{"key_id": "portal-key-1", "handle": "expiring-key", "api": apiRef(),
		"expires_at": "01/01/2027", "key": encryptField(t, secret)}
	err := h.handle(t, buildEvent(t, "evt-1", webhook.EventAPIKeyGenerated, data, "key"))
	if !IsPermanent(err) {
		t.Fatalf("Handle() with an unreadable expires_at = %v, want a permanent failure", err)
	}
	if !strings.Contains(err.Error(), "expires_at") {
		t.Errorf("the error does not name the field at fault: %v", err)
	}
	if fake.keyCount() != 0 {
		t.Error("a key was injected without the expiry the portal gave it")
	}
}

// --- other events and helpers -----------------------------------------------

func TestApplicationEventIsAcknowledgedWithoutAGatewayCall(t *testing.T) {
	h := newHarness(t, newFakeGateway(), nil)
	body := buildEvent(t, "evt-app-1", "application.created", map[string]any{
		"application_id": "portal-app-1", "display_name": "My App",
	})
	if err := h.handle(t, body); err != nil {
		t.Fatalf("Handle(application.created) = %v, want nil", err)
	}
}

func TestUnknownEventTypeIsAcknowledged(t *testing.T) {
	h := newHarness(t, newFakeGateway(), nil)
	body := buildEvent(t, "evt-x", "something.brand_new", map[string]any{"whatever": true})
	if err := h.handle(t, body); err != nil {
		t.Fatalf("Handle() for an unknown event type = %v, want nil", err)
	}
}

// TestEventsForOneSubscriptionShareAnOrderingKey asserts a create and the delete
// that follows it are serialized rather than applied concurrently.
func TestEventsForOneSubscriptionShareAnOrderingKey(t *testing.T) {
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
	if deletedKey := orderingKey(deletedEnv, deleted); createdKey != deletedKey {
		t.Fatalf("ordering keys differ: %q and %q", createdKey, deletedKey)
	}
}

func TestOrderingKeyFallsBackToTheEventID(t *testing.T) {
	body := buildEvent(t, "evt-1", "x.y", map[string]any{"nothing": "here"})
	env, _ := webhook.Decode(body)
	if got := orderingKey(env, body); got != "event:evt-1" {
		t.Fatalf("orderingKey() = %q, want the event-id fallback", got)
	}
}

func TestKeyedMutexSerializesOneKeyAndForgetsIt(t *testing.T) {
	var k keyedMutex
	unlock := k.lock("sub:1")

	acquired := make(chan struct{})
	go func() {
		release := k.lock("sub:1")
		close(acquired)
		release()
	}()
	select {
	case <-acquired:
		t.Fatal("a second holder acquired a held key")
	case <-time.After(50 * time.Millisecond):
	}

	otherDone := make(chan struct{})
	go func() {
		k.lock("sub:2")()
		close(otherDone)
	}()
	select {
	case <-otherDone:
	case <-time.After(time.Second):
		t.Fatal("an unrelated key waited on a held one")
	}

	unlock()
	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("the waiting holder never acquired the released key")
	}

	deadline := time.Now().Add(time.Second)
	for {
		k.mu.Lock()
		remaining := len(k.locks)
		k.mu.Unlock()
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d keys are still tracked after every holder released", remaining)
		}
		time.Sleep(5 * time.Millisecond)
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

func TestSubscriptionUpdateSendsOnlyTheSetFields(t *testing.T) {
	status, token, cleared, planID := "INACTIVE", "tok", "", "plan-1"
	for name, tc := range map[string]struct {
		update gateway.SubscriptionUpdate
		want   string
	}{
		"status":     {gateway.SubscriptionUpdate{Status: &status}, `{"status":"INACTIVE"}`},
		"token":      {gateway.SubscriptionUpdate{SubscriptionToken: &token}, `{"subscriptionToken":"tok"}`},
		"plan":       {gateway.SubscriptionUpdate{SubscriptionPlanID: &planID}, `{"subscriptionPlanId":"plan-1"}`},
		"clear plan": {gateway.SubscriptionUpdate{SubscriptionPlanID: &cleared}, `{"subscriptionPlanId":null}`},
	} {
		got, err := json.Marshal(tc.update)
		if err != nil {
			t.Fatalf("%s: Marshal() = %v", name, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s: body = %s, want %s", name, got, tc.want)
		}
	}
}

// --- API resolution --------------------------------------------------------

// subscribeTo sends a subscription.created for a portal API reference.
func subscribeTo(t *testing.T, id string, api map[string]any) []byte {
	return buildEvent(t, id, webhook.EventSubscriptionCreated, map[string]any{
		"subscription_id": "portal-sub-" + id, "subscriber_id": "dev@example.com", "status": "ACTIVE",
		"subscription_plan": map[string]string{"ref_id": "portal-plan-gold", "name": "Gold"},
		"api":               api, "token": encryptField(t, testToken+"-"+id),
	}, "token")
}

// TestRefIDWinsOverAMatchingDisplayName: the listing's ref_id names the gateway
// API exactly, so another gateway API that shares the portal's display name and
// version must not take the subscription.
func TestRefIDWinsOverAMatchingDisplayName(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "someone-elses-order-api-v1-0") // display name matches the portal's
	fake.addAPI("OrderAPI", "v1.0", "OrderAPI-v1.0")                 // the API the listing points to
	h := newHarness(t, fake, nil)

	api := map[string]any{"ref_id": "OrderAPI-v1.0", "name": "Order API", "version": "v1.0", "type": "RestApi"}
	if err := h.handle(t, subscribeTo(t, "evt-1", api)); err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if sub, _ := fake.onlySub(t); sub.APIID != "OrderAPI-v1.0" {
		t.Errorf("subscribed to %q, want the API the ref_id names", sub.APIID)
	}
}

// TestUnknownRefIDFallsBackToNameAndVersion: a ref_id that names nothing on the
// gateway (a listing published outside the catalog) still resolves by name.
func TestUnknownRefIDFallsBackToNameAndVersion(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("Order API", "v1.0", "order-api-v1.0")
	h := newHarness(t, fake, nil)

	api := map[string]any{"ref_id": "cp-api-1", "name": "Order API", "version": "v1.0", "type": "RestApi"}
	if err := h.handle(t, subscribeTo(t, "evt-1", api)); err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if sub, _ := fake.onlySub(t); sub.APIID != "order-api-v1.0" {
		t.Errorf("subscribed to %q, want the name and version match", sub.APIID)
	}
}

// TestRefIDResolvesWhenDisplayNamesDiffer: the portal's display name ("Air
// Quality API") needn't resemble the gateway's ("AirQualityAPI").
func TestRefIDResolvesWhenDisplayNamesDiffer(t *testing.T) {
	fake := newFakeGateway()
	fake.addAPI("AirQualityAPI", "v1.0", "AirQualityAPI-v1.0")
	h := newHarness(t, fake, nil)

	api := map[string]any{"ref_id": "AirQualityAPI-v1.0", "name": "Air Quality API", "version": "v1.0", "type": "RestApi"}
	if err := h.handle(t, subscribeTo(t, "evt-1", api)); err != nil {
		t.Fatalf("Handle() = %v", err)
	}
	if sub, _ := fake.onlySub(t); sub.APIID != "AirQualityAPI-v1.0" {
		t.Errorf("subscribed to %q, want AirQualityAPI-v1.0", sub.APIID)
	}
}
