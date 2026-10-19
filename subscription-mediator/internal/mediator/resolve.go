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
	// cacheKey is the key this resolution is cached under, so a caller holding a
	// resolvedAPI can invalidate or refresh it without rebuilding the key.
	cacheKey string
	// fromCache marks a resolution served from the cache rather than read from
	// the gateway during this call. Only a cached reading can be out of date.
	fromCache bool
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
	overrides map[string]string
	logger    *slog.Logger

	mu       sync.Mutex
	apiCache map[string]resolvedAPI
}

func newResolver(client *gateway.Client, overrides map[string]string, logger *slog.Logger) *resolver {
	return &resolver{
		client:    client,
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
		cached.fromCache = true
		return cached, nil
	}
	r.mu.Unlock()

	// 3. Resolve against the gateway's API list.
	handle, planNames, err := r.search(ctx, ref)
	if err != nil {
		return resolvedAPI{}, err
	}

	resolved := resolvedAPI{handle: handle, planNames: planNames, cacheKey: cacheKey, at: time.Now()}
	r.cache(cacheKey, resolved)
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
			resolved := resolvedAPI{handle: apis[i].Handle(), planNames: apis[i].Spec.SubscriptionPlans,
				cacheKey: cacheKey, at: time.Now()}
			r.cache(cacheKey, resolved)
			return resolved, nil
		}
	}
	return resolvedAPI{}, permanentf("the gateway has no REST API with handle %q", handle)
}

// search finds the gateway API matching a portal API reference.
//
// The listing's ref_id comes first: a publisher that knows the gateway API sets
// it to that API's name (the API catalog always does), so an exact match on it
// is authoritative and can't be confused with another API that happens to share
// the display name and version. Name and version matching is the fallback, for
// listings without a ref_id or whose ref_id names nothing on the gateway.
func (r *resolver) search(ctx context.Context, ref *webhook.APIRef) (string, []string, error) {
	name := strings.TrimSpace(ref.Name)
	version := strings.TrimSpace(ref.Version)

	if refID := strings.TrimSpace(ref.RefID); refID != "" {
		apis, err := r.client.ListRestAPIs(ctx, "", "")
		if err != nil {
			return "", nil, fmt.Errorf("failed to list the gateway's REST APIs: %w", err)
		}
		for i := range apis {
			if strings.EqualFold(apis[i].Handle(), refID) || strings.EqualFold(apis[i].Metadata.Name, refID) {
				return apis[i].Handle(), apis[i].Spec.SubscriptionPlans, nil
			}
		}
		r.logger.Info("no gateway API is named after the listing's ref_id; matching by name and version",
			slog.String("ref_id", refID), slog.String("portal_api", name+" "+version))
	}

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

// refreshAPI re-reads an API's plan allowlist from the gateway, bypassing the
// cached copy. The allowlist is the one part of a resolution that changes
// without any portal event to announce it — an operator can widen it on the
// gateway at any time — so a decision that turns on it must not be taken on a
// reading up to APICacheTTL old.
func (r *resolver) refreshAPI(ctx context.Context, resolved resolvedAPI) (resolvedAPI, error) {
	if resolved.handle == "" {
		return resolved, fmt.Errorf("cannot refresh an unresolved API")
	}
	r.invalidateAPI(resolved.cacheKey)
	return r.describeHandle(ctx, resolved.handle, resolved.cacheKey)
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

// resolvePlan maps a portal plan onto a gateway plan ID by name, creating the
// plan when the gateway does not have it.
//
// It returns an empty ID with a nil error when the plan cannot be mapped — the
// subscription then runs on the gateway's default limits rather than not at
// all, which keeps access working.
func (r *resolver) resolvePlan(ctx context.Context, plan *webhook.PlanRef) (string, error) {
	planName := plan.Display()
	if planName == "" {
		if strings.TrimSpace(plan.RefID) != "" {
			r.logger.Warn("the plan reference carries no name; the subscription will use the gateway's default limits")
		}
		return "", nil
	}

	// Match an existing gateway plan by name.
	if id, err := r.findPlan(ctx, planName); err != nil || id != "" {
		return id, err
	}

	// Create it.
	created, err := r.client.CreateSubscriptionPlan(ctx, planName)
	if err != nil {
		// A concurrent create loses the race and comes back 409; the winner's
		// plan is what both should use.
		if apiErr, ok := gateway.AsAPIError(err); ok && apiErr.IsConflict() {
			if id, findErr := r.findPlan(ctx, planName); findErr == nil && id != "" {
				return id, nil
			}
		}
		return "", fmt.Errorf("failed to create the gateway subscription plan %q: %w", planName, err)
	}
	r.logger.Info("created a gateway subscription plan for the portal plan",
		slog.String("plan_name", planName), slog.String("gateway_plan_id", created.ID))
	return created.ID, nil
}

// findPlan returns the ID of the gateway plan named planName, or "" when there
// is none.
func (r *resolver) findPlan(ctx context.Context, planName string) (string, error) {
	plans, err := r.client.ListSubscriptionPlans(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to list the gateway's subscription plans: %w", err)
	}
	for i := range plans {
		if strings.EqualFold(plans[i].PlanName, planName) {
			return plans[i].ID, nil
		}
	}
	return "", nil
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
