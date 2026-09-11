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

// The gateway's constraints on an injected API key: the value must be at least
// 36 characters, and the name must match ^[a-z0-9]+(-[a-z0-9]+)*$, 3..63 chars.
const (
	minInjectedKeyLength = 36
	keyNameMinLength     = 3
	keyNameMaxLength     = 63
)

// handleAPIKeyGenerated injects a portal-generated API key into the gateway.
// The portal owns key generation; the gateway stores the value's hash, so the
// key the developer copied from the portal is what authenticates.
func (m *Mediator) handleAPIKeyGenerated(ctx context.Context, env *webhook.Envelope, log *slog.Logger) error {
	data, err := decodeAPIKeyData(env)
	if err != nil {
		return err
	}
	secret, err := m.apiKeySecret(env, data)
	if err != nil {
		return err
	}
	resolved, err := m.resolver.resolveAPI(ctx, &data.API)
	if err != nil {
		return err
	}

	keyName := m.gatewayKeyName(data, resolved.handle)
	req := &gateway.APIKeyRequest{Name: keyName, APIKey: secret, ExternalRefID: data.KeyID}
	if expiresAt, ok := data.ExpiresAtTime(); ok {
		req.ExpiresAt = &expiresAt
	}

	created, err := m.client.CreateAPIKey(ctx, resolved.handle, req)
	if err != nil {
		apiErr, isAPIErr := gateway.AsAPIError(err)
		// A duplicate delivery, or a key of that name left by a previous partial
		// apply. Replacing the value converges on what the portal just issued.
		if isAPIErr && (apiErr.IsConflict() || strings.Contains(strings.ToLower(apiErr.Message), "already exists")) {
			log.Info("the gateway already has a key with this name; replacing its value",
				slog.String("gateway_key_name", keyName))
			updated, updateErr := m.client.UpdateAPIKey(ctx, resolved.handle, keyName, req)
			if updateErr != nil {
				return wrapKeyError("replace", resolved.handle, keyName, updateErr)
			}
			created = updated
		} else {
			return wrapKeyError("create", resolved.handle, keyName, err)
		}
	}

	if err := m.store.PutAPIKey(&store.APIKey{
		PortalKeyID:      data.KeyID,
		GatewayAPIHandle: resolved.handle,
		GatewayKeyName:   created.Name,
		PortalHandle:     strings.TrimSpace(data.Handle),
	}); err != nil {
		return fmt.Errorf("gateway API key %q was created but the mapping could not be persisted: %w", created.Name, err)
	}

	log.Info("injected the portal API key into the gateway",
		slog.String("portal_key_id", data.KeyID),
		slog.String("gateway_api_handle", resolved.handle),
		slog.String("gateway_key_name", created.Name))
	return nil
}

// handleAPIKeyRegenerated replaces an existing key's value with the rotated one.
func (m *Mediator) handleAPIKeyRegenerated(ctx context.Context, env *webhook.Envelope, log *slog.Logger) error {
	data, err := decodeAPIKeyData(env)
	if err != nil {
		return err
	}
	secret, err := m.apiKeySecret(env, data)
	if err != nil {
		return err
	}
	resolved, err := m.resolver.resolveAPI(ctx, &data.API)
	if err != nil {
		return err
	}

	keyName := ""
	if record, ok := m.store.APIKeyByPortalID(data.KeyID); ok && record.GatewayKeyName != "" {
		keyName = record.GatewayKeyName
	} else {
		// The generate delivery was missed. The gateway name is derived
		// deterministically from the portal handle, so it can still be found.
		keyName = m.gatewayKeyName(data, resolved.handle)
	}

	req := &gateway.APIKeyRequest{Name: keyName, APIKey: secret, ExternalRefID: data.KeyID}
	if expiresAt, ok := data.ExpiresAtTime(); ok {
		req.ExpiresAt = &expiresAt
	}

	updated, err := m.client.UpdateAPIKey(ctx, resolved.handle, keyName, req)
	if err != nil {
		apiErr, isAPIErr := gateway.AsAPIError(err)
		if isAPIErr && apiErr.IsNotFound() {
			// Nothing to rotate — creating it reaches the same end state, so a
			// missed generate delivery self-heals here.
			log.Info("the gateway has no key to rotate; creating it instead",
				slog.String("gateway_key_name", keyName))
			created, createErr := m.client.CreateAPIKey(ctx, resolved.handle, req)
			if createErr != nil {
				return wrapKeyError("create", resolved.handle, keyName, createErr)
			}
			updated = created
		} else {
			return wrapKeyError("replace", resolved.handle, keyName, err)
		}
	}

	if err := m.store.PutAPIKey(&store.APIKey{
		PortalKeyID:      data.KeyID,
		GatewayAPIHandle: resolved.handle,
		GatewayKeyName:   updated.Name,
		PortalHandle:     strings.TrimSpace(data.Handle),
	}); err != nil {
		log.Warn("failed to persist the rotated key mapping", slog.Any("error", err))
	}

	log.Info("rotated the gateway API key",
		slog.String("portal_key_id", data.KeyID),
		slog.String("gateway_key_name", updated.Name))
	return nil
}

// handleAPIKeyRevoked revokes the gateway key. This is the event that matters
// most for latency: until it is applied, a key the developer already revoked in
// the portal still authenticates at the gateway.
func (m *Mediator) handleAPIKeyRevoked(ctx context.Context, env *webhook.Envelope, log *slog.Logger) error {
	data, err := decodeAPIKeyData(env)
	if err != nil {
		return err
	}
	resolved, err := m.resolver.resolveAPI(ctx, &data.API)
	if err != nil {
		return err
	}

	keyName := ""
	if record, ok := m.store.APIKeyByPortalID(data.KeyID); ok && record.GatewayKeyName != "" {
		keyName = record.GatewayKeyName
	} else {
		keyName = m.gatewayKeyName(data, resolved.handle)
	}

	if err := m.client.RevokeAPIKey(ctx, resolved.handle, keyName); err != nil {
		apiErr, isAPIErr := gateway.AsAPIError(err)
		switch {
		case isAPIErr && apiErr.IsNotFound():
			// Already absent; the key cannot authenticate anything, which is what
			// the revocation asked for.
			log.Info("the gateway has no such key to revoke; already in the desired state",
				slog.String("gateway_key_name", keyName))
		case isAPIErr && apiErr.StatusCode < 500:
			return permanentf("the gateway rejected the key revocation: %s", apiErr.Message)
		default:
			return fmt.Errorf("failed to revoke gateway API key %q on API %q: %w", keyName, resolved.handle, err)
		}
	}

	if err := m.store.DeleteAPIKey(data.KeyID); err != nil {
		log.Warn("revoked the key but failed to forget its mapping", slog.Any("error", err))
	}
	log.Info("revoked the gateway API key",
		slog.String("portal_key_id", data.KeyID),
		slog.String("gateway_key_name", keyName))
	return nil
}

// decodeAPIKeyData decodes and checks an apikey.* payload.
func decodeAPIKeyData(env *webhook.Envelope) (*webhook.APIKeyData, error) {
	var data webhook.APIKeyData
	if err := env.DecodeData(&data); err != nil {
		return nil, permanentf("%v", err)
	}
	if strings.TrimSpace(data.KeyID) == "" {
		return nil, permanentf("data.key_id is required")
	}
	return &data, nil
}

// apiKeySecret decrypts the key value and checks it against the gateway's
// minimum length. Padding or truncating a credential to fit would silently break
// authentication, so a value the gateway will not accept is reported, not adjusted.
func (m *Mediator) apiKeySecret(env *webhook.Envelope, data *webhook.APIKeyData) (string, error) {
	if data.Key.Empty() {
		return "", permanentf("event %s carries no API key value: the gateway needs it to store the hash. "+
			"The API Portal only sends it when the subscriber has a secret configured — confirm the "+
			"subscriber's secret matches [webhook] secret in config.toml", env.EventType)
	}
	secret, err := m.codec.Decrypt(data.Key)
	if err != nil {
		return "", permanentf("failed to decrypt the API key value: %v", err)
	}
	if len(secret) < minInjectedKeyLength {
		return "", permanentf("the portal issued a %d-character key; the gateway requires at least %d for an "+
			"injected key. Padding or truncating it would break authentication, so it is not applied",
			len(secret), minInjectedKeyLength)
	}
	return secret, nil
}

// gatewayKeyName derives the gateway key name from the portal's key handle.
//
// The gateway uses the name as a path parameter and constrains its characters,
// so the handle is slugified. The derivation is deterministic, which is what
// lets a rotate or revoke find the key again even without a stored mapping.
func (m *Mediator) gatewayKeyName(data *webhook.APIKeyData, apiHandle string) string {
	base := slugify(data.Handle)
	if base == "" {
		base = slugify(data.DisplayName)
	}
	if base == "" {
		base = slugify(data.KeyID) // a UUID is already name-safe once slugified
	}
	if base == "" {
		base = "portal-key"
	}
	if len(base) > keyNameMaxLength {
		base = strings.Trim(base[:keyNameMaxLength], "-")
	}
	for len(base) < keyNameMinLength {
		base += "-k"
	}

	// Two portal keys whose handles slugify identically would otherwise collide
	// on one gateway key and silently overwrite each other.
	if m.store.GatewayKeyNameTaken(apiHandle, base, data.KeyID) {
		suffix := slugify(data.KeyID)
		if len(suffix) > 8 {
			suffix = suffix[:8]
		}
		if suffix == "" {
			suffix = "dup"
		}
		if trimTo := keyNameMaxLength - len(suffix) - 1; trimTo > 0 && len(base) > trimTo {
			base = strings.Trim(base[:trimTo], "-")
		}
		base = base + "-" + suffix
	}
	return base
}

// wrapKeyError classifies a gateway API key failure.
func wrapKeyError(action, apiHandle, keyName string, err error) error {
	if apiErr, ok := gateway.AsAPIError(err); ok {
		if apiErr.IsNotFound() {
			return fmt.Errorf("the gateway has no API %q for the key %s: %w", apiHandle, action, err)
		}
		if apiErr.StatusCode < 500 {
			return permanentf("the gateway rejected the API key %s for %q on API %q: %s",
				action, keyName, apiHandle, apiErr.Message)
		}
	}
	return fmt.Errorf("failed to %s gateway API key %q on API %q: %w", action, keyName, apiHandle, err)
}
