package mediator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"subscription-mediator/internal/gateway"
	"subscription-mediator/internal/store"
	"subscription-mediator/internal/webhook"
)

// handleSubscriptionCreated translates subscription.created into a gateway
// subscription. The token arrives encrypted; the gateway stores only its hash,
// so the token the developer sees in the portal is what validates at the gateway.
func (m *Mediator) handleSubscriptionCreated(ctx context.Context, env *webhook.Envelope, log *slog.Logger) error {
	var data webhook.SubscriptionData
	if err := env.DecodeData(&data); err != nil {
		return permanentf("%v", err)
	}
	if strings.TrimSpace(data.SubscriptionID) == "" {
		return permanentf("data.subscription_id is required")
	}

	token, err := m.subscriptionToken(env, &data)
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

	// Pre-empt the gateway's own check: an API that declares a plan allowlist
	// rejects a subscription whose plan is not on it. Dropping the plan keeps the
	// subscription working on default limits instead of failing the event.
	planName := data.Plan.Display()
	if planID != "" && planName != "" && !planEnabledForAPI(resolved, planName) {
		log.Warn("the plan is not enabled for this gateway API; creating the subscription without a plan",
			slog.String("plan_name", planName), slog.String("gateway_api_handle", resolved.handle))
		planID = ""
	}

	status := normalizeStatus(data.Status)
	req := &gateway.SubscriptionCreateRequest{
		APIID:              resolved.handle,
		SubscriptionToken:  token,
		ApplicationID:      strings.TrimSpace(data.ApplicationID),
		SubscriptionPlanID: planID,
		Status:             status,
	}
	created, err := m.createSubscription(ctx, req, planName, resolved, log)
	if err != nil {
		return err
	}

	record := &store.Subscription{
		PortalSubscriptionID:  data.SubscriptionID,
		GatewaySubscriptionID: created.ID,
		GatewayAPIHandle:      resolved.handle,
		GatewayPlanID:         req.SubscriptionPlanID,
		PortalPlanRefID:       strings.TrimSpace(data.Plan.RefID),
		PortalPlanName:        planName,
		ApplicationID:         req.ApplicationID,
		SubscriberID:          strings.TrimSpace(data.SubscriberID),
		Status:                status,
		APIName:               strings.TrimSpace(data.API.Name),
		APIVersion:            strings.TrimSpace(data.API.Version),
		Token:                 token,
	}
	if err := m.store.PutSubscription(record); err != nil {
		// The gateway already has the subscription; failing here would retry the
		// create. Surface it as what it is: the mapping is lost, so later
		// lifecycle events for this subscription cannot be applied.
		return fmt.Errorf("gateway subscription %s was created but the mapping could not be persisted: %w", created.ID, err)
	}

	log.Info("created a gateway subscription",
		slog.String("portal_subscription_id", data.SubscriptionID),
		slog.String("gateway_subscription_id", created.ID),
		slog.String("gateway_api_handle", resolved.handle),
		slog.String("gateway_plan_id", req.SubscriptionPlanID),
		slog.String("status", status))
	return nil
}

// createSubscription creates the subscription, absorbing the two outcomes that
// are not real failures: the gateway rejecting the plan, and a duplicate
// delivery finding the subscription already there.
func (m *Mediator) createSubscription(
	ctx context.Context,
	req *gateway.SubscriptionCreateRequest,
	planName string,
	resolved resolvedAPI,
	log *slog.Logger,
) (*gateway.Subscription, error) {
	created, err := m.client.CreateSubscription(ctx, req)
	if err == nil {
		return created, nil
	}

	apiErr, isAPIErr := gateway.AsAPIError(err)
	if !isAPIErr {
		return nil, fmt.Errorf("failed to create the gateway subscription: %w", err)
	}

	// The gateway validates the plan against the API's allowlist and status.
	// Retry once without it.
	if apiErr.IsBadRequest() && req.SubscriptionPlanID != "" &&
		strings.Contains(strings.ToLower(apiErr.Message), "subscription plan") {
		log.Warn("the gateway rejected the subscription plan; retrying without it",
			slog.String("plan_name", planName), slog.String("gateway_message", apiErr.Message))
		retry := *req
		retry.SubscriptionPlanID = ""
		created, retryErr := m.client.CreateSubscription(ctx, &retry)
		if retryErr == nil {
			req.SubscriptionPlanID = ""
			return created, nil
		}
		converted, ok := gateway.AsAPIError(retryErr)
		if !ok {
			return nil, fmt.Errorf("failed to create the gateway subscription without a plan: %w", retryErr)
		}
		apiErr = converted
	}

	// A duplicate delivery, or a create whose response was lost after the
	// gateway committed it. Adopt the existing subscription so state converges.
	if apiErr.IsConflict() {
		subs, listErr := m.client.ListSubscriptions(ctx, req.APIID, req.ApplicationID)
		if listErr != nil {
			return nil, fmt.Errorf("the gateway reported the subscription exists but listing it failed: %w", listErr)
		}
		if len(subs) == 1 {
			log.Info("the gateway subscription already existed; adopting it",
				slog.String("gateway_subscription_id", subs[0].ID))
			return &subs[0], nil
		}
		return nil, permanentf("the gateway reported the subscription already exists and %d match API %q "+
			"application %q — resolve the duplicate on the gateway first", len(subs), req.APIID, req.ApplicationID)
	}

	if apiErr.IsNotFound() {
		m.resolver.invalidateAPI(resolved.handle)
		return nil, fmt.Errorf("the gateway no longer has API %q: %w", resolved.handle, err)
	}
	if apiErr.StatusCode < 500 {
		return nil, permanentf("the gateway rejected the subscription create: %s", apiErr.Message)
	}
	return nil, fmt.Errorf("failed to create the gateway subscription: %w", err)
}

// handleSubscriptionUpdated applies a status change (ACTIVE <-> INACTIVE).
func (m *Mediator) handleSubscriptionUpdated(ctx context.Context, env *webhook.Envelope, log *slog.Logger) error {
	var data webhook.SubscriptionData
	if err := env.DecodeData(&data); err != nil {
		return permanentf("%v", err)
	}
	if strings.TrimSpace(data.Status) == "" {
		return permanentf("data.status is required for %s", env.EventType)
	}

	record, err := m.locate(ctx, &data, log)
	if err != nil {
		return err
	}

	status := normalizeStatus(data.Status)
	if err := m.client.UpdateSubscriptionStatus(ctx, record.GatewaySubscriptionID, status); err != nil {
		if apiErr, ok := gateway.AsAPIError(err); ok && apiErr.IsNotFound() {
			if delErr := m.store.DeleteSubscription(record.PortalSubscriptionID); delErr != nil {
				log.Warn("failed to forget the mapping for a subscription the gateway no longer has",
					slog.Any("error", delErr))
			}
			return permanentf("the gateway no longer has subscription %s", record.GatewaySubscriptionID)
		}
		if apiErr, ok := gateway.AsAPIError(err); ok && apiErr.StatusCode < 500 {
			return permanentf("the gateway rejected the status change: %s", apiErr.Message)
		}
		return fmt.Errorf("failed to update the gateway subscription status: %w", err)
	}

	record.Status = status
	if err := m.store.PutSubscription(record); err != nil {
		log.Warn("failed to persist the updated status", slog.Any("error", err))
	}
	log.Info("updated the gateway subscription status",
		slog.String("gateway_subscription_id", record.GatewaySubscriptionID),
		slog.String("status", status))
	return nil
}

// handleSubscriptionPlanChanged applies a plan switch.
//
// The gateway's subscription update accepts only a status change, so the plan is
// changed by deleting the subscription and recreating it with the same token
// under the new plan. That is why the stored record keeps the token.
func (m *Mediator) handleSubscriptionPlanChanged(ctx context.Context, env *webhook.Envelope, log *slog.Logger) error {
	var data webhook.SubscriptionData
	if err := env.DecodeData(&data); err != nil {
		return permanentf("%v", err)
	}

	record, err := m.locate(ctx, &data, log)
	if err != nil {
		return err
	}
	if record.Token == "" {
		return permanentf("cannot apply a plan change for portal subscription %s: the gateway has no "+
			"plan-change operation, so the subscription must be recreated with its existing token, and no "+
			"token is stored for it. Have the developer regenerate the subscription token instead",
			record.PortalSubscriptionID)
	}

	planID, err := m.resolver.resolvePlan(ctx, &data.Plan)
	if err != nil {
		return err
	}
	return m.recreate(ctx, record, &data, record.Token, planID, log, "plan change")
}

// handleSubscriptionTokenRegenerated applies a token rotation. Like a plan
// change this is a delete followed by a create, because the gateway stores only
// the token's hash and offers no way to replace it in place.
func (m *Mediator) handleSubscriptionTokenRegenerated(ctx context.Context, env *webhook.Envelope, log *slog.Logger) error {
	var data webhook.SubscriptionData
	if err := env.DecodeData(&data); err != nil {
		return permanentf("%v", err)
	}

	token, err := m.subscriptionToken(env, &data)
	if err != nil {
		return err
	}
	record, err := m.locate(ctx, &data, log)
	if err != nil {
		return err
	}

	planID := record.GatewayPlanID
	if strings.TrimSpace(data.Plan.RefID) != "" || data.Plan.Display() != "" {
		if resolvedPlanID, err := m.resolver.resolvePlan(ctx, &data.Plan); err == nil {
			planID = resolvedPlanID
		} else if !isPermanent(err) {
			return err
		}
	}
	return m.recreate(ctx, record, &data, token, planID, log, "token regeneration")
}

// recreate deletes the current gateway subscription and creates a replacement.
//
// The delete is recorded before the create is attempted, so a retry after a
// partial failure resumes at the create rather than deleting again.
func (m *Mediator) recreate(
	ctx context.Context,
	record *store.Subscription,
	data *webhook.SubscriptionData,
	token, planID string,
	log *slog.Logger,
	reason string,
) error {
	resolved, err := m.resolver.resolveAPI(ctx, &data.API)
	if err != nil {
		return err
	}

	if record.GatewaySubscriptionID != "" {
		if err := m.client.DeleteSubscription(ctx, record.GatewaySubscriptionID); err != nil {
			if apiErr, ok := gateway.AsAPIError(err); !ok || !apiErr.IsNotFound() {
				return fmt.Errorf("failed to delete gateway subscription %s before recreating it for a %s: %w",
					record.GatewaySubscriptionID, reason, err)
			}
		}
		record.GatewaySubscriptionID = ""
		if err := m.store.PutSubscription(record); err != nil {
			return fmt.Errorf("deleted the gateway subscription for a %s but could not record it: %w", reason, err)
		}
	}

	planName := data.Plan.Display()
	if planID != "" && planName != "" && !planEnabledForAPI(resolved, planName) {
		log.Warn("the new plan is not enabled for this gateway API; recreating without a plan",
			slog.String("plan_name", planName))
		planID = ""
	}

	status := record.Status
	if s := normalizeStatus(data.Status); s != "" {
		status = s
	}
	req := &gateway.SubscriptionCreateRequest{
		APIID:              resolved.handle,
		SubscriptionToken:  token,
		ApplicationID:      record.ApplicationID,
		SubscriptionPlanID: planID,
		Status:             status,
	}
	created, err := m.createSubscription(ctx, req, planName, resolved, log)
	if err != nil {
		return err
	}

	record.GatewaySubscriptionID = created.ID
	record.GatewayAPIHandle = resolved.handle
	record.GatewayPlanID = req.SubscriptionPlanID
	record.PortalPlanRefID = strings.TrimSpace(data.Plan.RefID)
	record.PortalPlanName = planName
	record.Status = status
	record.Token = token
	if err := m.store.PutSubscription(record); err != nil {
		return fmt.Errorf("recreated gateway subscription %s for a %s but could not persist the mapping: %w",
			created.ID, reason, err)
	}

	log.Info("recreated the gateway subscription",
		slog.String("reason", reason),
		slog.String("gateway_subscription_id", created.ID),
		slog.String("gateway_plan_id", req.SubscriptionPlanID),
		slog.String("status", status))
	return nil
}

// handleSubscriptionDeleted removes the gateway subscription.
func (m *Mediator) handleSubscriptionDeleted(ctx context.Context, env *webhook.Envelope, log *slog.Logger) error {
	var data webhook.SubscriptionData
	if err := env.DecodeData(&data); err != nil {
		return permanentf("%v", err)
	}

	record, ok := m.store.Subscription(data.SubscriptionID)
	if !ok {
		// Nothing mapped. Try to identify it on the gateway; if that is
		// inconclusive there is nothing to revoke and the end state already holds.
		adopted, err := m.adopt(ctx, &data, log)
		if err != nil {
			if isPermanent(err) {
				log.Warn("no gateway subscription could be identified for a delete; nothing to revoke",
					slog.String("portal_subscription_id", data.SubscriptionID))
				return nil
			}
			return err
		}
		record = adopted
	}

	if record.GatewaySubscriptionID != "" {
		if err := m.client.DeleteSubscription(ctx, record.GatewaySubscriptionID); err != nil {
			apiErr, ok := gateway.AsAPIError(err)
			switch {
			case ok && apiErr.IsNotFound():
				// Already gone; the end state is what the event asked for.
			case ok && apiErr.StatusCode < 500:
				return permanentf("the gateway rejected the subscription delete: %s", apiErr.Message)
			default:
				return fmt.Errorf("failed to delete gateway subscription %s: %w", record.GatewaySubscriptionID, err)
			}
		}
	}

	if err := m.store.DeleteSubscription(record.PortalSubscriptionID); err != nil {
		log.Warn("deleted the gateway subscription but failed to forget its mapping", slog.Any("error", err))
	}
	log.Info("deleted the gateway subscription",
		slog.String("gateway_subscription_id", record.GatewaySubscriptionID))
	return nil
}

// locate finds the mediator's record, recovering from a missed create.
func (m *Mediator) locate(ctx context.Context, data *webhook.SubscriptionData, log *slog.Logger) (*store.Subscription, error) {
	if record, ok := m.store.Subscription(data.SubscriptionID); ok {
		return record, nil
	}
	return m.adopt(ctx, data, log)
}

// adopt identifies an unmapped portal subscription on the gateway.
//
// The portal delivers each event exactly once with no retry, so a missed
// subscription.created leaves later events with nothing to act on. When the API
// and application narrow it to exactly one gateway subscription, that is
// unambiguous enough to adopt.
func (m *Mediator) adopt(ctx context.Context, data *webhook.SubscriptionData, log *slog.Logger) (*store.Subscription, error) {
	resolved, err := m.resolver.resolveAPI(ctx, &data.API)
	if err != nil {
		return nil, err
	}
	applicationID := strings.TrimSpace(data.ApplicationID)
	subs, err := m.client.ListSubscriptions(ctx, resolved.handle, applicationID)
	if err != nil {
		return nil, fmt.Errorf("failed to list gateway subscriptions while identifying %s: %w",
			data.SubscriptionID, err)
	}
	if len(subs) != 1 {
		return nil, permanentf("portal subscription %s is not mapped and %d gateway subscriptions match "+
			"API %q application %q, so it cannot be identified. This happens when the subscription.created "+
			"delivery was missed (the API Portal does not redeliver). Have the developer regenerate the "+
			"subscription token to resend one this mediator can apply",
			data.SubscriptionID, len(subs), resolved.handle, applicationID)
	}

	adopted := &store.Subscription{
		PortalSubscriptionID:  data.SubscriptionID,
		GatewaySubscriptionID: subs[0].ID,
		GatewayAPIHandle:      resolved.handle,
		GatewayPlanID:         subs[0].SubscriptionPlanID,
		ApplicationID:         applicationID,
		SubscriberID:          strings.TrimSpace(data.SubscriberID),
		Status:                subs[0].Status,
		APIName:               strings.TrimSpace(data.API.Name),
		APIVersion:            strings.TrimSpace(data.API.Version),
	}
	if err := m.store.PutSubscription(adopted); err != nil {
		return nil, fmt.Errorf("failed to persist the adopted mapping: %w", err)
	}
	log.Info("adopted an existing gateway subscription for an unmapped portal subscription",
		slog.String("gateway_subscription_id", subs[0].ID))
	return adopted, nil
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
