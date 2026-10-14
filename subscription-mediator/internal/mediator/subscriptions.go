package mediator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"subscription-mediator/internal/gateway"
	"subscription-mediator/internal/webhook"
)

// handleSubscriptionCreated translates subscription.created into a gateway
// subscription. The token arrives encrypted; the gateway stores only its hash,
// so the token the developer sees in the portal is what validates at the gateway.
func (m *Mediator) handleSubscriptionCreated(ctx context.Context, env *webhook.Envelope, log *slog.Logger) error {
	data, err := decodeSubscriptionData(env)
	if err != nil {
		return err
	}
	token, err := m.subscriptionToken(env, data)
	if err != nil {
		return err
	}
	return m.createSubscription(ctx, data, token, log)
}

// createSubscription creates the gateway subscription for a portal one, under
// the portal's subscription ID as its reference ID.
func (m *Mediator) createSubscription(ctx context.Context, data *webhook.SubscriptionData, token string, log *slog.Logger) error {
	resolved, err := m.resolver.resolveAPI(ctx, &data.API)
	if err != nil {
		return err
	}
	planID, err := m.resolver.resolvePlan(ctx, &data.Plan)
	if err != nil {
		return err
	}

	// Pre-empt the gateway's own check: an API that declares a plan allowlist
	// rejects a subscription whose plan is not on it. Dropping the plan keeps the
	// subscription working on default limits instead of failing the event.
	planName := data.Plan.Display()
	if planID != "" && planName != "" {
		var allowed bool
		if resolved, allowed = m.planAllowed(ctx, resolved, planName, log); !allowed {
			log.Warn("the plan is not enabled for this gateway API; creating the subscription without a plan",
				slog.String("plan_name", planName), slog.String("gateway_api_handle", resolved.handle))
			planID = ""
		}
	}

	req := &gateway.SubscriptionCreateRequest{
		ReferenceID:        data.SubscriptionID,
		APIID:              resolved.handle,
		SubscriptionToken:  token,
		ApplicationID:      strings.TrimSpace(data.ApplicationID),
		SubscriptionPlanID: planID,
		Status:             normalizeStatus(data.Status),
	}
	created, err := m.client.CreateSubscription(ctx, req)
	if err != nil {
		created, err = m.recoverCreate(ctx, req, planName, resolved, err, log)
		if err != nil {
			return err
		}
		if created == nil {
			return nil // it already existed
		}
	}

	log.Info("created a gateway subscription",
		slog.String("portal_subscription_id", data.SubscriptionID),
		slog.String("gateway_subscription_id", created.ID),
		slog.String("gateway_api_handle", resolved.handle),
		slog.String("gateway_plan_id", req.SubscriptionPlanID),
		slog.String("status", req.Status))
	return nil
}

// recoverCreate handles a failed create. Two outcomes are not real failures:
// the gateway rejecting the plan, which is retried without one, and a duplicate
// delivery finding the subscription already there, which returns (nil, nil).
func (m *Mediator) recoverCreate(
	ctx context.Context,
	req *gateway.SubscriptionCreateRequest,
	planName string,
	resolved resolvedAPI,
	err error,
	log *slog.Logger,
) (*gateway.Subscription, error) {
	apiErr, isAPIErr := gateway.AsAPIError(err)
	if !isAPIErr {
		return nil, fmt.Errorf("failed to create the gateway subscription: %w", err)
	}

	if isPlanRejection(apiErr) && req.SubscriptionPlanID != "" {
		log.Warn("the gateway rejected the subscription plan; retrying without it",
			slog.String("plan_name", planName), slog.String("gateway_message", apiErr.Message))
		req.SubscriptionPlanID = ""
		created, retryErr := m.client.CreateSubscription(ctx, req)
		if retryErr == nil {
			return created, nil
		}
		converted, ok := gateway.AsAPIError(retryErr)
		if !ok {
			return nil, fmt.Errorf("failed to create the gateway subscription without a plan: %w", retryErr)
		}
		apiErr, err = converted, retryErr
	}

	// A duplicate delivery, or a create whose response was lost after the
	// gateway committed it. The reference ID says whether it is ours.
	if apiErr.IsConflict() {
		existing, findErr := m.client.FindSubscription(ctx, req.ReferenceID)
		if findErr != nil {
			return nil, fmt.Errorf("the gateway reported the subscription exists but looking it up failed: %w", findErr)
		}
		if existing != nil {
			log.Info("the gateway already has this subscription; nothing to do",
				slog.String("portal_subscription_id", req.ReferenceID),
				slog.String("gateway_subscription_id", existing.ID))
			return nil, nil
		}
		return nil, permanentf("the gateway rejected the subscription create as a conflict, and holds no "+
			"subscription with reference ID %q: %s", req.ReferenceID, apiErr.Message)
	}

	if apiErr.IsNotFound() {
		m.resolver.invalidateAPI(resolved.cacheKey)
		return nil, permanentf("the gateway no longer has API %q: %s", resolved.handle, apiErr.Message)
	}
	if apiErr.StatusCode < 500 {
		return nil, permanentf("the gateway rejected the subscription create: %s", apiErr.Message)
	}
	return nil, fmt.Errorf("failed to create the gateway subscription: %w", err)
}

// planAllowed reports whether planName is on the gateway API's plan allowlist,
// and returns the resolution the answer was read from.
//
// A negative is confirmed against a fresh read of the API. The allowlist is
// cached for APICacheTTL and nothing on the portal side announces a change to
// it, so a plan an operator enabled on the gateway moments ago can still look
// disabled here. Acting on that stale reading drops the plan — which costs the
// subscription its rate limit until some later event happens to reinstate it —
// so it is worth one extra call to the gateway to be sure.
func (m *Mediator) planAllowed(
	ctx context.Context,
	resolved resolvedAPI,
	planName string,
	log *slog.Logger,
) (resolvedAPI, bool) {
	if planEnabledForAPI(resolved, planName) {
		return resolved, true
	}
	if !resolved.fromCache {
		// Read from the gateway while handling this event. Reading it again
		// would return the same answer.
		return resolved, false
	}
	fresh, err := m.resolver.refreshAPI(ctx, resolved)
	if err != nil {
		// The cached allowlist is all there is to go on. Treating the plan as
		// disabled matches what the gateway would do with it anyway.
		log.Warn("could not re-read the gateway API's plan allowlist; deciding on the cached one",
			slog.String("gateway_api_handle", resolved.handle), slog.Any("error", err))
		return resolved, false
	}
	if planEnabledForAPI(fresh, planName) {
		log.Info("the plan is enabled for this gateway API on a fresh read; the cached allowlist was stale",
			slog.String("plan_name", planName), slog.String("gateway_api_handle", fresh.handle))
		return fresh, true
	}
	return fresh, false
}

// handleSubscriptionUpdated applies a status change (ACTIVE <-> INACTIVE).
func (m *Mediator) handleSubscriptionUpdated(ctx context.Context, env *webhook.Envelope, log *slog.Logger) error {
	data, err := decodeSubscriptionData(env)
	if err != nil {
		return err
	}
	if strings.TrimSpace(data.Status) == "" {
		return permanentf("data.status is required for %s", env.EventType)
	}

	sub, err := m.requireSubscription(ctx, data, env.EventType)
	if err != nil {
		return err
	}

	status := normalizeStatus(data.Status)
	if err := m.client.UpdateSubscription(ctx, sub.ID, gateway.SubscriptionUpdate{Status: &status}); err != nil {
		return classifyUpdateError("status change", sub.ID, err)
	}
	log.Info("updated the gateway subscription status",
		slog.String("gateway_subscription_id", sub.ID),
		slog.String("status", status))
	return nil
}

// handleSubscriptionPlanChanged switches the subscription's plan in place. The
// token is untouched, so the event does not need to carry it.
func (m *Mediator) handleSubscriptionPlanChanged(ctx context.Context, env *webhook.Envelope, log *slog.Logger) error {
	data, err := decodeSubscriptionData(env)
	if err != nil {
		return err
	}
	sub, err := m.requireSubscription(ctx, data, env.EventType)
	if err != nil {
		return err
	}

	resolved, err := m.resolver.resolveAPI(ctx, &data.API)
	if err != nil {
		return err
	}
	planID, err := m.resolver.resolvePlan(ctx, &data.Plan)
	if err != nil {
		return err
	}
	planName := data.Plan.Display()
	if planID != "" && planName != "" {
		if _, allowed := m.planAllowed(ctx, resolved, planName, log); !allowed {
			log.Warn("the new plan is not enabled for this gateway API; clearing the subscription's plan",
				slog.String("plan_name", planName), slog.String("gateway_api_handle", resolved.handle))
			planID = ""
		}
	}

	err = m.client.UpdateSubscription(ctx, sub.ID, gateway.SubscriptionUpdate{SubscriptionPlanID: &planID})
	if apiErr, ok := gateway.AsAPIError(err); ok && isPlanRejection(apiErr) && planID != "" {
		log.Warn("the gateway rejected the new plan; clearing the subscription's plan",
			slog.String("plan_name", planName), slog.String("gateway_message", apiErr.Message))
		planID = ""
		err = m.client.UpdateSubscription(ctx, sub.ID, gateway.SubscriptionUpdate{SubscriptionPlanID: &planID})
	}
	if err != nil {
		return classifyUpdateError("plan change", sub.ID, err)
	}

	log.Info("changed the gateway subscription's plan",
		slog.String("gateway_subscription_id", sub.ID),
		slog.String("gateway_plan_id", planID))
	return nil
}

// handleSubscriptionTokenRegenerated replaces the subscription's token in
// place. The event carries everything a create needs, so a subscription the
// gateway does not have — its created delivery was lost — is created here.
func (m *Mediator) handleSubscriptionTokenRegenerated(ctx context.Context, env *webhook.Envelope, log *slog.Logger) error {
	data, err := decodeSubscriptionData(env)
	if err != nil {
		return err
	}
	token, err := m.subscriptionToken(env, data)
	if err != nil {
		return err
	}

	sub, err := m.findSubscription(ctx, data.SubscriptionID)
	if err != nil {
		return err
	}
	if sub == nil {
		log.Info("the gateway has no subscription to rotate the token of; creating it",
			slog.String("portal_subscription_id", data.SubscriptionID))
		return m.createSubscription(ctx, data, token, log)
	}

	if err := m.client.UpdateSubscription(ctx, sub.ID, gateway.SubscriptionUpdate{SubscriptionToken: &token}); err != nil {
		return classifyUpdateError("token rotation", sub.ID, err)
	}
	log.Info("replaced the gateway subscription's token",
		slog.String("gateway_subscription_id", sub.ID))
	return nil
}

// handleSubscriptionDeleted removes the gateway subscription.
func (m *Mediator) handleSubscriptionDeleted(ctx context.Context, env *webhook.Envelope, log *slog.Logger) error {
	data, err := decodeSubscriptionData(env)
	if err != nil {
		return err
	}
	sub, err := m.findSubscription(ctx, data.SubscriptionID)
	if err != nil {
		return err
	}
	if sub == nil {
		// Nothing to revoke; the end state is what the event asked for.
		log.Info("the gateway has no such subscription; already in the desired state",
			slog.String("portal_subscription_id", data.SubscriptionID))
		return nil
	}

	if err := m.client.DeleteSubscription(ctx, sub.ID); err != nil {
		apiErr, ok := gateway.AsAPIError(err)
		switch {
		case ok && apiErr.IsNotFound():
			// Deleted between the lookup and now.
		case ok && apiErr.StatusCode < 500:
			return permanentf("the gateway rejected the subscription delete: %s", apiErr.Message)
		default:
			return fmt.Errorf("failed to delete gateway subscription %s: %w", sub.ID, err)
		}
	}
	log.Info("deleted the gateway subscription",
		slog.String("portal_subscription_id", data.SubscriptionID),
		slog.String("gateway_subscription_id", sub.ID))
	return nil
}

// decodeSubscriptionData decodes and checks a subscription.* payload.
func decodeSubscriptionData(env *webhook.Envelope) (*webhook.SubscriptionData, error) {
	var data webhook.SubscriptionData
	if err := env.DecodeData(&data); err != nil {
		return nil, permanentf("%v", err)
	}
	data.SubscriptionID = strings.TrimSpace(data.SubscriptionID)
	if data.SubscriptionID == "" {
		return nil, permanentf("data.subscription_id is required")
	}
	return &data, nil
}

// findSubscription looks a portal subscription up on the gateway by its
// reference ID, returning nil when the gateway has none.
func (m *Mediator) findSubscription(ctx context.Context, portalSubscriptionID string) (*gateway.Subscription, error) {
	sub, err := m.client.FindSubscription(ctx, portalSubscriptionID)
	if err != nil {
		if apiErr, ok := gateway.AsAPIError(err); ok && apiErr.StatusCode < 500 {
			return nil, permanentf("the gateway rejected the subscription lookup: %s", apiErr.Message)
		}
		return nil, fmt.Errorf("failed to look up the gateway subscription for %s: %w", portalSubscriptionID, err)
	}
	return sub, nil
}

// requireSubscription is findSubscription for an event that can only change an
// existing subscription.
func (m *Mediator) requireSubscription(ctx context.Context, data *webhook.SubscriptionData, eventType string) (*gateway.Subscription, error) {
	sub, err := m.findSubscription(ctx, data.SubscriptionID)
	if err != nil {
		return nil, err
	}
	if sub == nil {
		return nil, permanentf("cannot apply %s: the gateway has no subscription with reference ID %q. Its "+
			"subscription.created delivery was probably missed (the API Portal does not redeliver). Have the "+
			"developer regenerate the subscription token, which sends an event this mediator can create it from",
			eventType, data.SubscriptionID)
	}
	return sub, nil
}

// subscriptionToken decrypts the token an event carries.
//
// The portal omits it entirely when the webhook subscriber has no secret
// configured — but this mediator requires one, so an absent token means the
// subscriber registered in the portal is not the one whose secret this holds.
func (m *Mediator) subscriptionToken(env *webhook.Envelope, data *webhook.SubscriptionData) (string, error) {
	if data.Token.Empty() {
		return "", permanentf("event %s carries no subscription token: the gateway requires one. The API "+
			"Portal only sends it when the subscriber has a secret configured — confirm the subscriber's "+
			"secret matches [webhook] secret in config.toml", env.EventType)
	}
	token, err := m.codec.Decrypt(data.Token)
	if err != nil {
		return "", permanentf("failed to decrypt the subscription token: %v", err)
	}
	if strings.TrimSpace(token) == "" {
		return "", permanentf("the decrypted subscription token is empty")
	}
	return token, nil
}

// isPlanRejection reports the gateway refusing a plan for an API — its
// allowlist or the plan's status.
func isPlanRejection(apiErr *gateway.APIError) bool {
	return apiErr.IsBadRequest() && strings.Contains(strings.ToLower(apiErr.Message), "subscription plan")
}

// classifyUpdateError turns a failed subscription update into a permanent or
// transient error.
func classifyUpdateError(action, subscriptionID string, err error) error {
	if apiErr, ok := gateway.AsAPIError(err); ok {
		if apiErr.IsNotFound() {
			return permanentf("the gateway no longer has subscription %s for the %s", subscriptionID, action)
		}
		if apiErr.StatusCode < 500 {
			return permanentf("the gateway rejected the %s: %s", action, apiErr.Message)
		}
	}
	return fmt.Errorf("failed to apply the %s to gateway subscription %s: %w", action, subscriptionID, err)
}

// normalizeStatus maps a portal status onto one the gateway accepts. Statuses
// are compared as upper case, so a lower-case value cannot miss the enum.
func normalizeStatus(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case gateway.StatusActive:
		return gateway.StatusActive
	case gateway.StatusInactive:
		return gateway.StatusInactive
	case gateway.StatusRevoked:
		return gateway.StatusRevoked
	case "":
		return ""
	default:
		// An unrecognised status must not widen access.
		return gateway.StatusInactive
	}
}
