package mediator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

	"subscription-mediator/internal/gateway"
	"subscription-mediator/internal/store"
	"subscription-mediator/internal/webhook"
)

// KindRestAPI is the only artifact kind the gateway's subscription and API-key
// operations accept — its handler rejects a configuration of any other kind.
const KindRestAPI = "RestApi"

// resolvedAPI is a cached portal-API to gateway-API resolution.
type resolvedAPI struct {
	handle string
	// planNames is the API's own subscription-plan allowlist. When non-empty the
	// gateway rejects a subscription whose plan is not in it.
	planNames []string
	at        time.Time
}

// resolver maps portal identifiers onto gateway identifiers.
//
// The portal's api.ref_id is a control-plane API UUID. A gateway fed by a
// different control plane — WSO2 API Manager, in the deployment this service
// exists for — has never seen that UUID, so the resolution that actually works
// is on the API's display name and version, against the gateway's own API list.
type resolver struct {
	client    *gateway.Client
	store     *store.Store
	overrides map[string]string
	logger    *slog.Logger

	mu       sync.Mutex
	apiCache map[string]resolvedAPI
}

func newResolver(client *gateway.Client, st *store.Store, overrides map[string]string, logger *slog.Logger) *resolver {
	return &resolver{
		client:    client,
		store:     st,
		overrides: overrides,
		logger:    logger,
		apiCache:  map[string]resolvedAPI{},
	}
}

// resolveAPI returns the gateway API handle and plan allowlist for a portal API
// reference. A permanentError means no retry can help.
func (r *resolver) resolveAPI(ctx context.Context, ref *webhook.APIRef) (resolvedAPI, error) {
	if kind := ref.Kind(); kind != "" && !strings.EqualFold(kind, KindRestAPI) {
		return resolvedAPI{}, permanentf("the gateway supports subscriptions and API keys for %s only; "+
			"this event targets kind %q, which has no equivalent gateway operation", KindRestAPI, kind)
	}

	cacheKey := apiCacheKey(ref)

	// 1. Explicit operator override, by ref_id or "<name>:<version>".
	if handle, ok := r.override(ref); ok {
		return r.describeHandle(ctx, handle, cacheKey)
	}

	// 2. Cached resolution, still within its TTL.
	r.mu.Lock()
	cached, ok := r.apiCache[cacheKey]
	if ok && time.Since(cached.at) < APICacheTTL {
		r.mu.Unlock()
		return cached, nil
	}
	r.mu.Unlock()

	// 3. A resolution persisted by an earlier run.
	if handle, ok := r.store.APIMapping(cacheKey); ok && handle != "" {
		resolved, err := r.describeHandle(ctx, handle, cacheKey)
		if err == nil {
			return resolved, nil
		}
		if !isPermanent(err) {
			return resolvedAPI{}, err
		}
		r.logger.Info("stored API mapping no longer resolves on the gateway; re-resolving",
			slog.String("portal_api_ref", cacheKey), slog.String("stale_handle", handle))
	}

	// 4. Resolve against the gateway's API list.
	handle, planNames, err := r.search(ctx, ref)
	if err != nil {
		return resolvedAPI{}, err
	}

	resolved := resolvedAPI{handle: handle, planNames: planNames, at: time.Now()}
	r.cache(cacheKey, resolved)
	if err := r.store.PutAPIMapping(cacheKey, handle); err != nil {
		// The mapping is a cache, not the source of truth: failing to persist it
		// costs a re-resolution later, so it must not fail the event.
		r.logger.Warn("failed to persist the API mapping", slog.Any("error", err))
	}
	r.logger.Info("resolved portal API to gateway API",
		slog.String("portal_api", ref.Name+" "+ref.Version),
		slog.String("gateway_api_handle", handle))
	return resolved, nil
}

// apiCacheKey prefers the portal ref_id, which is stable across a rename.
func apiCacheKey(ref *webhook.APIRef) string {
	if id := strings.TrimSpace(ref.RefID); id != "" {
		return id
	}
	return strings.ToLower(strings.TrimSpace(ref.Name) + ":" + strings.TrimSpace(ref.Version))
}

// override looks the reference up in the config's [api_handles] map.
func (r *resolver) override(ref *webhook.APIRef) (string, bool) {
	if len(r.overrides) == 0 {
		return "", false
	}
	for _, candidate := range []string{
		strings.ToLower(strings.TrimSpace(ref.RefID)),
		strings.ToLower(strings.TrimSpace(ref.Name) + ":" + strings.TrimSpace(ref.Version)),
		strings.ToLower(strings.TrimSpace(ref.Name)),
	} {
		if candidate == "" || candidate == ":" {
			continue
		}
		if handle, ok := r.overrides[candidate]; ok && handle != "" {
			return handle, true
		}
	}
	return "", false
}

// describeHandle confirms a handle exists on the gateway and reads its plan
// allowlist. The result is cached, so this runs once per API per TTL.
func (r *resolver) describeHandle(ctx context.Context, handle, cacheKey string) (resolvedAPI, error) {
	apis, err := r.client.ListRestAPIs(ctx, "", "")
	if err != nil {
		return resolvedAPI{}, fmt.Errorf("failed to list the gateway's REST APIs: %w", err)
	}
	for i := range apis {
		if strings.EqualFold(apis[i].Handle(), handle) || strings.EqualFold(apis[i].Metadata.Name, handle) {
			resolved := resolvedAPI{handle: apis[i].Handle(), planNames: apis[i].Spec.SubscriptionPlans, at: time.Now()}
			r.cache(cacheKey, resolved)
			return resolved, nil
		}
	}
	return resolvedAPI{}, permanentf("the gateway has no REST API with handle %q", handle)
}

// search finds the gateway API matching a portal API reference.
func (r *resolver) search(ctx context.Context, ref *webhook.APIRef) (string, []string, error) {
	name := strings.TrimSpace(ref.Name)
	version := strings.TrimSpace(ref.Version)

	// The gateway filters on displayName and version directly — the exact match
	// and the cheapest call.
	if name != "" && version != "" {
		apis, err := r.client.ListRestAPIs(ctx, name, version)
		if err != nil {
			return "", nil, fmt.Errorf("failed to list the gateway's REST APIs: %w", err)
		}
		if len(apis) == 1 {
			return apis[0].Handle(), apis[0].Spec.SubscriptionPlans, nil
		}
		if len(apis) > 1 {
			return "", nil, permanentf("portal API %q version %q matches %d gateway APIs; "+
				"pin it under [api_handles] in config.toml", name, version, len(apis))
		}
	}

	// No exact hit. Scan the full list and match more loosely: the gateway's
	// display name can differ from the portal's in case, spacing or punctuation
	// when the two were populated from different control planes.
	apis, err := r.client.ListRestAPIs(ctx, "", "")
	if err != nil {
		return "", nil, fmt.Errorf("failed to list the gateway's REST APIs: %w", err)
	}

	type candidate struct {
		handle    string
		planNames []string
	}
	var byHandle, normalized []candidate
	wantName := normalizeName(name)
	wantVersion := normalizeVersion(version)

	for i := range apis {
		api := &apis[i]
		c := candidate{handle: api.Handle(), planNames: api.Spec.SubscriptionPlans}

		// The gateway's own handle convention is "<slugified name>-<version>".
		if wantName != "" && wantVersion != "" && strings.EqualFold(api.Handle(), slugify(name)+"-"+version) {
			byHandle = append(byHandle, c)
			continue
		}
		if ref.RefID != "" && strings.EqualFold(api.Handle(), ref.RefID) {
			byHandle = append(byHandle, c)
			continue
		}
		if wantName != "" && normalizeName(api.Spec.DisplayName) == wantName &&
			wantVersion != "" && normalizeVersion(api.Spec.Version) == wantVersion {
			normalized = append(normalized, c)
		}
	}

	for _, set := range [][]candidate{byHandle, normalized} {
		if len(set) == 1 {
			return set[0].handle, set[0].planNames, nil
		}
		if len(set) > 1 {
			return "", nil, permanentf("portal API %q version %q matches %d gateway APIs ambiguously; "+
				"pin it under [api_handles] in config.toml", name, version, len(set))
		}
	}

	return "", nil, permanentf("no gateway REST API matches portal API %q version %q; deploy it to the "+
		"gateway first, or pin it in config.toml with [api_handles] %q = \"<gateway-api-handle>\"",
		name, version, strings.ToLower(name+":"+version))
}

func (r *resolver) cache(key string, resolved resolvedAPI) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.apiCache[key] = resolved
}

// invalidateAPI drops a cached resolution, so the next event re-resolves.
func (r *resolver) invalidateAPI(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.apiCache, key)
}

// planEnabledForAPI reports whether a plan name is acceptable for an API. An API
// with an empty allowlist accepts any plan.
func planEnabledForAPI(resolved resolvedAPI, planName string) bool {
	if len(resolved.planNames) == 0 {
		return true
	}
	for _, name := range resolved.planNames {
		if strings.EqualFold(name, planName) {
			return true
		}
	}
	return false
}

// resolvePlan maps a portal plan reference onto a gateway plan ID, creating the
// plan when the gateway does not have it.
//
// It returns an empty ID with a nil error when the plan cannot be mapped — the
// subscription is then created on the gateway's default limits rather than not
// at all, which keeps access working.
func (r *resolver) resolvePlan(ctx context.Context, plan *webhook.PlanRef) (string, error) {
	refID := strings.TrimSpace(plan.RefID)
	planName := plan.Display()
	if refID == "" && planName == "" {
		return "", nil
	}

	// A mapping from an earlier resolution, re-checked so a plan deleted on the
	// gateway does not keep being referenced.
	if refID != "" {
		if id, ok := r.store.PlanMapping(refID); ok && id != "" {
			_, err := r.client.GetSubscriptionPlan(ctx, id)
			if err == nil {
				return id, nil
			}
			if apiErr, ok := gateway.AsAPIError(err); ok && apiErr.IsNotFound() {
				r.logger.Info("the mapped gateway plan no longer exists; re-resolving",
					slog.String("gateway_plan_id", id))
				if err := r.store.DeletePlanMapping(refID); err != nil {
					r.logger.Warn("failed to forget the stale plan mapping", slog.Any("error", err))
				}
			} else {
				return "", fmt.Errorf("failed to verify the gateway subscription plan: %w", err)
			}
		}
	}

	if planName == "" {
		r.logger.Warn("the plan reference carries no name; the subscription will use the gateway's default limits")
		return "", nil
	}

	// Match an existing gateway plan by name.
	plans, err := r.client.ListSubscriptionPlans(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to list the gateway's subscription plans: %w", err)
	}
	for i := range plans {
		if strings.EqualFold(plans[i].PlanName, planName) {
			r.rememberPlan(refID, plans[i].ID)
			return plans[i].ID, nil
		}
	}

	// Create it.
	created, err := r.client.CreateSubscriptionPlan(ctx, planName)
	if err != nil {
		// A concurrent create loses the race and comes back 409; the winner's
		// plan is what both should use.
		if apiErr, ok := gateway.AsAPIError(err); ok && apiErr.IsConflict() {
			if plans, listErr := r.client.ListSubscriptionPlans(ctx); listErr == nil {
				for i := range plans {
					if strings.EqualFold(plans[i].PlanName, planName) {
						r.rememberPlan(refID, plans[i].ID)
						return plans[i].ID, nil
					}
				}
			}
		}
		return "", fmt.Errorf("failed to create the gateway subscription plan %q: %w", planName, err)
	}
	r.logger.Info("created a gateway subscription plan for the portal plan",
		slog.String("plan_name", planName), slog.String("gateway_plan_id", created.ID))
	r.rememberPlan(refID, created.ID)
	return created.ID, nil
}

func (r *resolver) rememberPlan(refID, gatewayPlanID string) {
	if refID == "" || gatewayPlanID == "" {
		return
	}
	if err := r.store.PutPlanMapping(refID, gatewayPlanID); err != nil {
		r.logger.Warn("failed to persist the plan mapping", slog.Any("error", err))
	}
}

// normalizeName folds a display name to letters and digits in lower case, so
// "Order API", "order-api" and "OrderAPI" compare equal.
func normalizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// normalizeVersion folds a version so "v1.0" and "1.0" compare equal.
func normalizeVersion(s string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "v")
}

// slugify renders a display name the way the gateway's handle convention does.
func slugify(s string) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(unicode.ToLower(r))
			lastHyphen = false
		default:
			if !lastHyphen && b.Len() > 0 {
				b.WriteRune('-')
				lastHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
