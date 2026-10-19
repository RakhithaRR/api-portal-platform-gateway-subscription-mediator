// Package webhook implements the consumer side of the WSO2 API Portal 1.0.0
// webhook contract: signature verification, envelope decoding, and AES-256-GCM
// field decryption.
//
// The wire contract is the one documented in the API Portal 1.0.0 webhook
// integration guide, and this implementation is byte-compatible with the
// Platform API's own receiver, so both consume the same deliveries.
package webhook

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha3"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Event types carried in the envelope's event_type field.
const (
	EventAPIKeyGenerated          = "apikey.generated"
	EventAPIKeyRegenerated        = "apikey.regenerated"
	EventAPIKeyRevoked            = "apikey.revoked"
	EventAPIKeyApplicationUpdated = "apikey.application_updated"

	EventSubscriptionCreated          = "subscription.created"
	EventSubscriptionUpdated          = "subscription.updated"
	EventSubscriptionPlanChanged      = "subscription.plan_changed"
	EventSubscriptionTokenRegenerated = "subscription.token_regenerated"
	EventSubscriptionDeleted          = "subscription.deleted"
)

// ErrSignature means the delivery was not authentic: missing, malformed, stale,
// or wrongly signed. One error for every cause, so nothing distinguishes them.
var ErrSignature = errors.New("webhook signature is missing or invalid")

// fieldKeyInfo is the HKDF label the producer uses to derive the
// field-encryption key from the shared secret. It must stay byte-identical to
// the API Portal's own label (src/services/webhooks/envelopeCrypto.js).
const fieldKeyInfo = "api-portal-webhook-field-encryption-v1"

// --- envelope --------------------------------------------------------------

// EncryptedField is the AES-256-GCM envelope the portal uses for a sensitive
// value. iv is the 12-byte GCM nonce, tag the 16-byte tag, all base64.
type EncryptedField struct {
	IV         string `json:"iv"`
	Tag        string `json:"tag"`
	Ciphertext string `json:"ciphertext"`
}

// Empty reports whether the field is unset or incomplete.
func (e *EncryptedField) Empty() bool {
	return e == nil || e.IV == "" || e.Tag == "" || e.Ciphertext == ""
}

// APIRef identifies the API an event concerns.
//
// RefID is the control-plane API UUID as the portal knows it. A gateway fed by
// a different control plane (WSO2 API Manager, in the setup this service exists
// for) has never seen that UUID, so Name plus Version is what actually gets
// matched against the gateway's own API list.
type APIRef struct {
	RefID   string `json:"ref_id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Type    string `json:"type"`
}

// Kind returns the artifact kind (RestApi, Mcp, ...).
func (a *APIRef) Kind() string { return strings.TrimSpace(a.Type) }

// PlanRef identifies a subscription plan. The documented field for the name is
// "name"; the Platform API receiver also accepts "plan_name", so both are read.
type PlanRef struct {
	RefID    string `json:"ref_id"`
	Name     string `json:"name"`
	PlanName string `json:"plan_name"`
}

// Display returns the plan name from whichever field carried it.
func (p *PlanRef) Display() string {
	if n := strings.TrimSpace(p.Name); n != "" {
		return n
	}
	return strings.TrimSpace(p.PlanName)
}

// Envelope is the common webhook event envelope.
type Envelope struct {
	EventID    string `json:"event_id"`
	EventType  string `json:"event_type"`
	OccurredAt string `json:"occurred_at"`
	Org        struct {
		RefID string `json:"ref_id"`
	} `json:"org"`
	EncryptedFields []string        `json:"encrypted_fields"`
	Data            json.RawMessage `json:"data"`
}

// SubscriptionData is the data payload for every subscription.* event.
type SubscriptionData struct {
	SubscriptionID string          `json:"subscription_id"`
	SubscriberID   string          `json:"subscriber_id"`
	Status         string          `json:"status"`
	Plan           PlanRef         `json:"subscription_plan"`
	API            APIRef          `json:"api"`
	ApplicationID  string          `json:"application_id"`
	Token          *EncryptedField `json:"token"`
}

// APIKeyData is the data payload for every apikey.* event.
type APIKeyData struct {
	KeyID       string          `json:"key_id"`
	Handle      string          `json:"handle"`
	DisplayName string          `json:"display_name"`
	ExpiresAt   *string         `json:"expires_at"`
	API         APIRef          `json:"api"`
	Key         *EncryptedField `json:"key"`
}

// ExpiresAtTime parses expires_at. A nil result with a nil error is a
// non-expiring key (the field is null or absent). A value that is present but
// not an RFC 3339 timestamp is an error: treating it as "no expiry" would inject
// a key that never expires where the portal meant one that does.
func (d *APIKeyData) ExpiresAtTime() (*time.Time, error) {
	if d.ExpiresAt == nil || strings.TrimSpace(*d.ExpiresAt) == "" {
		return nil, nil
	}
	raw := strings.TrimSpace(*d.ExpiresAt)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return &t, nil
		}
	}
	return nil, fmt.Errorf("expires_at %q is not an RFC 3339 timestamp", raw)
}

// Decode parses a raw request body into an Envelope and checks the fields the
// mediator needs.
func Decode(body []byte) (*Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("invalid webhook envelope: %w", err)
	}
	if strings.TrimSpace(env.EventID) == "" || strings.TrimSpace(env.EventType) == "" {
		return nil, fmt.Errorf("invalid webhook envelope: event_id and event_type are required")
	}
	return &env, nil
}

// DecodeData unmarshals the event-specific payload into out.
func (e *Envelope) DecodeData(out any) error {
	if len(e.Data) == 0 {
		return fmt.Errorf("event %s carries no data", e.EventType)
	}
	if err := json.Unmarshal(e.Data, out); err != nil {
		return fmt.Errorf("invalid data for event %s: %w", e.EventType, err)
	}
	return nil
}

// --- verification and decryption -------------------------------------------

// Codec verifies delivery signatures and decrypts the sensitive fields. Both
// come from the one shared subscriber secret; the encryption key is derived from
// it with HKDF-SHA3-256 so it is domain-separated from the signing key.
type Codec struct {
	secret   []byte
	fieldKey []byte
}

// NewCodec derives the field-decryption key from the shared webhook secret.
func NewCodec(secret string) (*Codec, error) {
	if secret == "" {
		return nil, fmt.Errorf("webhook secret is required")
	}
	key, err := hkdf.Key(sha3.New256, []byte(secret), nil, fieldKeyInfo, 32)
	if err != nil {
		// Deliberately does not include the secret or any derivative of it.
		return nil, fmt.Errorf("failed to derive the webhook field-decryption key: %w", err)
	}
	return &Codec{secret: []byte(secret), fieldKey: key}, nil
}

// Verify checks the "t=<unix>,v1=<hex>" signature header against the raw body.
// The MAC covers "<t>.<raw body>", so the body must be passed exactly as
// received — before any decode or re-encode.
func (c *Codec) Verify(header string, body []byte, now time.Time) error {
	var tsStr, sig string
	for _, part := range strings.Split(header, ",") {
		key, value, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "t":
			tsStr = strings.TrimSpace(value)
		case "v1":
			sig = strings.TrimSpace(value)
		}
	}
	if tsStr == "" || sig == "" {
		return ErrSignature
	}
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return ErrSignature
	}
	// Reject a stale or future-dated signature, so an old delivery cannot be
	// replayed.
	if delta := now.Sub(time.Unix(ts, 0)); delta > 300*time.Second || delta < -300*time.Second {
		return ErrSignature
	}

	mac := hmac.New(sha256.New, c.secret)
	mac.Write([]byte(tsStr))
	mac.Write([]byte("."))
	mac.Write(body)
	got, err := hex.DecodeString(sig)
	if err != nil || !hmac.Equal(mac.Sum(nil), got) {
		return ErrSignature
	}
	return nil
}

// Decrypt returns the plaintext of an encrypted field. The producer sends the
// GCM tag separately from the ciphertext, while Go expects it appended.
func (c *Codec) Decrypt(field *EncryptedField) (string, error) {
	if field.Empty() {
		return "", fmt.Errorf("encrypted field is empty")
	}
	iv, err := base64.StdEncoding.DecodeString(field.IV)
	if err != nil {
		return "", fmt.Errorf("iv is not valid base64")
	}
	ciphertext, err := base64.StdEncoding.DecodeString(field.Ciphertext)
	if err != nil {
		return "", fmt.Errorf("ciphertext is not valid base64")
	}
	tag, err := base64.StdEncoding.DecodeString(field.Tag)
	if err != nil {
		return "", fmt.Errorf("tag is not valid base64")
	}

	block, err := aes.NewCipher(c.fieldKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(iv) != gcm.NonceSize() {
		return "", fmt.Errorf("iv must be %d bytes, got %d", gcm.NonceSize(), len(iv))
	}

	sealed := make([]byte, 0, len(ciphertext)+len(tag))
	sealed = append(sealed, ciphertext...)
	sealed = append(sealed, tag...)
	plaintext, err := gcm.Open(nil, iv, sealed, nil)
	if err != nil {
		return "", fmt.Errorf("decryption failed — the subscriber's secret in the API Portal must match [webhook] secret in config.toml")
	}
	return string(plaintext), nil
}
