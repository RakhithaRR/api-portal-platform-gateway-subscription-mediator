package mediator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"subscription-mediator/internal/gateway"
	"subscription-mediator/internal/webhook"
)

// The gateway's constraints on an injected API key: the value must be at least
// 36 characters, and the name must match ^[a-z0-9]+(-[a-z0-9]+)*$, 3..63 chars.
const (
	minInjectedKeyLength = 36
	keyNameMaxLength     = 63
)

// handleAPIKeyGenerated injects a portal-generated API key into the gateway.
// The portal owns key generation; the gateway stores the value's hash, so the
// key the developer copied from the portal is what authenticates.
func (m *Mediator) handleAPIKeyGenerated(ctx context.Context, env *webhook.Envelope, log *slog.Logger) error {
	return m.putAPIKey(ctx, env, log, "injected the portal API key into the gateway")
}

// handleAPIKeyRegenerated replaces an existing key's value with the rotated one.
// When the gateway has no such key — its generate delivery was lost — it is
// created, which reaches the same end state.
func (m *Mediator) handleAPIKeyRegenerated(ctx context.Context, env *webhook.Envelope, log *slog.Logger) error {
	return m.putAPIKey(ctx, env, log, "rotated the gateway API key")
}

// putAPIKey makes the gateway hold the event's key value under the portal key:
// it replaces the value of the live key carrying the portal key ID, or creates
// one. A generate and a regenerate differ only in which outcome is expected, so
// a duplicate or a lost delivery of either converges.
func (m *Mediator) putAPIKey(ctx context.Context, env *webhook.Envelope, log *slog.Logger, done string) error {
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
	keys, err := m.listAPIKeys(ctx, resolved.handle)
	if err != nil {
		return err
	}

	// An expiry the mediator cannot read is reported, not dropped: a key injected
	// without one would outlive what the developer was shown in the portal.
	expiresAt, err := data.ExpiresAtTime()
	if err != nil {
		return permanentf("the key's expiry cannot be applied: %v", err)
	}
	req := &gateway.APIKeyRequest{APIKey: secret, ExternalRefID: data.KeyID, ExpiresAt: expiresAt}

	var key *gateway.APIKey
	if existing := liveKeyFor(keys, data.KeyID); existing != nil {
		req.Name = existing.Name
		key, err = m.client.UpdateAPIKey(ctx, resolved.handle, existing.Name, req)
		if err != nil {
			return wrapKeyError("replace", resolved.handle, existing.Name, err)
		}
	} else {
		req.Name, err = gatewayKeyName(data, keys)
		if err != nil {
			return err
		}
		key, err = m.client.CreateAPIKey(ctx, resolved.handle, req)
		if err != nil {
			return wrapKeyError("create", resolved.handle, req.Name, err)
		}
	}

	log.Info(done,
		slog.String("portal_key_id", data.KeyID),
		slog.String("gateway_api_handle", resolved.handle),
		slog.String("gateway_key_name", key.Name))
	return nil
}

// handleAPIKeyRevoked revokes the gateway key. Until it is applied, a key the
// developer already revoked in the portal still authenticates at the gateway.
func (m *Mediator) handleAPIKeyRevoked(ctx context.Context, env *webhook.Envelope, log *slog.Logger) error {
	data, err := decodeAPIKeyData(env)
	if err != nil {
		return err
	}
	resolved, err := m.resolver.resolveAPI(ctx, &data.API)
	if err != nil {
		return err
	}
	keys, err := m.listAPIKeys(ctx, resolved.handle)
	if err != nil {
		return err
	}

	key := liveKeyFor(keys, data.KeyID)
	if key == nil {
		// Absent or already revoked; the key cannot authenticate anything,
		// which is what the revocation asked for.
		log.Info("the gateway has no live key for this portal key; already in the desired state",
			slog.String("portal_key_id", data.KeyID))
		return nil
	}

	if err := m.client.RevokeAPIKey(ctx, resolved.handle, key.Name); err != nil {
		apiErr, isAPIErr := gateway.AsAPIError(err)
		switch {
		case isAPIErr && apiErr.IsNotFound():
			// Removed between the listing and now.
		case isAPIErr && apiErr.StatusCode < 500:
			return permanentf("the gateway rejected the key revocation: %s", apiErr.Message)
		default:
			return fmt.Errorf("failed to revoke gateway API key %q on API %q: %w", key.Name, resolved.handle, err)
		}
	}
	log.Info("revoked the gateway API key",
		slog.String("portal_key_id", data.KeyID),
		slog.String("gateway_key_name", key.Name))
	return nil
}

// listAPIKeys reads every key on a gateway API, revoked ones included.
func (m *Mediator) listAPIKeys(ctx context.Context, apiHandle string) ([]gateway.APIKey, error) {
	keys, err := m.client.ListAPIKeys(ctx, apiHandle)
	if err != nil {
		if apiErr, ok := gateway.AsAPIError(err); ok && apiErr.StatusCode < 500 {
			return nil, permanentf("the gateway rejected listing the API keys of %q: %s", apiHandle, apiErr.Message)
		}
		return nil, fmt.Errorf("failed to list the API keys of gateway API %q: %w", apiHandle, err)
	}
	return keys, nil
}

// liveKeyFor returns the key for the portal key ID that is not revoked: the one
// named after it (gatewayKeyName), or, for a key created before keys were named
// that way, the one carrying it as externalRefId. The name is what lasts: the
// gateway clears a key's externalRefId when the key is updated (a regenerate).
func liveKeyFor(keys []gateway.APIKey, portalKeyID string) *gateway.APIKey {
	name := keyNameFor(portalKeyID)
	for i := range keys {
		if (keys[i].Name == name || keys[i].ExternalRefID == portalKeyID) &&
			!strings.EqualFold(keys[i].Status, gateway.KeyStatusRevoked) {
			return &keys[i]
		}
	}
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

// gatewayKeyName picks the gateway name for a new key: derived from the portal
// key ID alone, so every later event for the key can find it by name.
//
// The gateway uses the name as a path parameter and constrains its characters,
// so the ID is slugified. A portal key ID names one key for its whole life, so
// the name is only ever taken by that same key — revoked, since a live one would
// have been found and updated instead. Creating it again would revive a revoked
// credential, so that is refused.
func gatewayKeyName(data *webhook.APIKeyData, keys []gateway.APIKey) (string, error) {
	name := keyNameFor(data.KeyID)
	if keyNameTaken(keys, name) {
		return "", permanentf("portal key %s was already revoked on the gateway (key %q); a revoked key is not "+
			"recreated", data.KeyID, name)
	}
	return name, nil
}

// keyNameFor is the gateway key name for a portal key ID: "pk-" and the slugified
// ID (a UUID fits in full), within the gateway's 3..63 characters.
func keyNameFor(portalKeyID string) string {
	name := "pk-" + slugify(portalKeyID)
	if len(name) > keyNameMaxLength {
		name = strings.Trim(name[:keyNameMaxLength], "-")
	}
	return name
}

func keyNameTaken(keys []gateway.APIKey, name string) bool {
	for i := range keys {
		if keys[i].Name == name {
			return true
		}
	}
	return false
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
