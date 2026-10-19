package webhook

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha3"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"testing"
	"time"
)

const testSecret = "a-webhook-subscriber-secret-at-least-32-chars"

// signBody reproduces the API Portal's signing scheme, so the verifier is
// tested against the producer's algorithm rather than its own implementation.
func signBody(t *testing.T, secret string, ts int64, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10) + "." + string(body)))
	return fmt.Sprintf("t=%d,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
}

// encryptField reproduces the API Portal's field encryption.
func encryptField(t *testing.T, secret, plaintext string) *EncryptedField {
	t.Helper()
	key, err := hkdf.Key(sha3.New256, []byte(secret), nil, fieldKeyInfo, 32)
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
	return &EncryptedField{
		IV:         base64.StdEncoding.EncodeToString(iv),
		Tag:        base64.StdEncoding.EncodeToString(sealed[len(sealed)-16:]),
		Ciphertext: base64.StdEncoding.EncodeToString(sealed[:len(sealed)-16]),
	}
}

func newTestCodec(t *testing.T, secret string) *Codec {
	t.Helper()
	c, err := NewCodec(secret)
	if err != nil {
		t.Fatalf("NewCodec() = %v", err)
	}
	return c
}

// TestVerifyAcceptsAGenuineSignature pins compatibility with the producer's
// HMAC scheme: the MAC is over "<timestamp>.<raw body>".
func TestVerifyAcceptsAGenuineSignature(t *testing.T) {
	body := []byte(`{"event_id":"e1","event_type":"subscription.created"}`)
	now := time.Now()
	c := newTestCodec(t, testSecret)
	if err := c.Verify(signBody(t, testSecret, now.Unix(), body), body, now); err != nil {
		t.Fatalf("Verify() on a genuine signature = %v, want nil", err)
	}
}

func TestVerifyRejectsBadSignatures(t *testing.T) {
	body := []byte(`{"event_id":"e1","status":"ACTIVE"}`)
	now := time.Now()
	c := newTestCodec(t, testSecret)

	cases := map[string]struct {
		header string
		body   []byte
	}{
		"no header":        {"", body},
		"garbage header":   {"not-a-signature", body},
		"missing v1":       {fmt.Sprintf("t=%d", now.Unix()), body},
		"bad timestamp":    {"t=abc,v1=ff", body},
		"wrong secret":     {signBody(t, "a-completely-different-secret-32b!", now.Unix(), body), body},
		"tampered body":    {signBody(t, testSecret, now.Unix(), body), []byte(`{"event_id":"e1","status":"REVOKED"}`)},
		"stale timestamp":  {signBody(t, testSecret, now.Add(-2*time.Hour).Unix(), body), body},
		"future timestamp": {signBody(t, testSecret, now.Add(2*time.Hour).Unix(), body), body},
	}
	for name, tc := range cases {
		if err := c.Verify(tc.header, tc.body, now); err == nil {
			t.Errorf("Verify() with %s = nil, want an error", name)
		}
	}
}

// TestDecryptRecoversAProducerEncryptedToken is the interop test that matters:
// the token has to come back byte-identical or the gateway stores a hash of the
// wrong value and nothing authenticates.
func TestDecryptRecoversAProducerEncryptedToken(t *testing.T) {
	const token = "sub-token-8f3c1a9b-4d2e-4f60-9c11-7a5e2b8d0417"
	c := newTestCodec(t, testSecret)
	got, err := c.Decrypt(encryptField(t, testSecret, token))
	if err != nil {
		t.Fatalf("Decrypt() = %v", err)
	}
	if got != token {
		t.Fatalf("Decrypt() = %q, want %q", got, token)
	}
}

func TestDecryptRejectsBadInput(t *testing.T) {
	c := newTestCodec(t, testSecret)

	// A different secret derives a different key.
	other := newTestCodec(t, "another-subscriber-secret-of-32-chars!")
	if _, err := other.Decrypt(encryptField(t, testSecret, "sub-token-value")); err == nil {
		t.Error("Decrypt() with the wrong secret = nil, want an error")
	}

	// A tampered tag must fail the GCM check.
	field := encryptField(t, testSecret, "sub-token-value")
	field.Tag = base64.StdEncoding.EncodeToString(make([]byte, 16))
	if _, err := c.Decrypt(field); err == nil {
		t.Error("Decrypt() with a tampered tag = nil, want an error")
	}

	for name, f := range map[string]*EncryptedField{
		"nil":           nil,
		"empty":         {},
		"no ciphertext": {IV: "aaaa", Tag: "bbbb"},
		"bad base64 iv": {IV: "!!!", Tag: "YWFhYWFhYWFhYWFhYWFhYQ==", Ciphertext: "YWFhYQ=="},
		"short iv":      {IV: base64.StdEncoding.EncodeToString([]byte("short")), Tag: base64.StdEncoding.EncodeToString(make([]byte, 16)), Ciphertext: "YWFhYQ=="},
	} {
		if _, err := c.Decrypt(f); err == nil {
			t.Errorf("Decrypt() with a %s field = nil, want an error", name)
		}
	}
}

func TestNewCodecRequiresASecret(t *testing.T) {
	if _, err := NewCodec(""); err == nil {
		t.Fatal(`NewCodec("") = nil error, want a failure`)
	}
}

func TestDecodeAndDecodeData(t *testing.T) {
	body := []byte(`{
	  "event_id": "3f1a",
	  "event_type": "subscription.created",
	  "org": {"ref_id": "cp-org"},
	  "encrypted_fields": ["token"],
	  "data": {"subscription_id": "sub-1",
	           "api": {"ref_id": "api-1", "name": "Order API", "version": "v1.0", "type": "RestApi"}}
	}`)
	env, err := Decode(body)
	if err != nil {
		t.Fatalf("Decode() = %v", err)
	}
	var data SubscriptionData
	if err := env.DecodeData(&data); err != nil {
		t.Fatalf("DecodeData() = %v", err)
	}
	if data.SubscriptionID != "sub-1" || data.API.Kind() != "RestApi" {
		t.Fatalf("decoded data = %+v", data)
	}
}

func TestDecodeRejectsIncompleteEnvelopes(t *testing.T) {
	for name, body := range map[string]string{
		"not json":      `{{{`,
		"no event_id":   `{"event_type":"subscription.created"}`,
		"no event_type": `{"event_id":"e"}`,
	} {
		if _, err := Decode([]byte(body)); err == nil {
			t.Errorf("Decode() with %s = nil, want an error", name)
		}
	}
}

// TestPlanRefDisplayAcceptsBothProducerFieldNames pins tolerance for the two
// field names seen in the wild: the documented "name" and "plan_name".
func TestPlanRefDisplayAcceptsBothProducerFieldNames(t *testing.T) {
	if got := (&PlanRef{Name: "Gold"}).Display(); got != "Gold" {
		t.Errorf(`Display() with name = %q`, got)
	}
	if got := (&PlanRef{PlanName: "Silver"}).Display(); got != "Silver" {
		t.Errorf(`Display() with plan_name = %q`, got)
	}
}

func TestAPIKeyDataExpiresAt(t *testing.T) {
	iso := "2027-01-01T00:00:00Z"
	got, err := (&APIKeyData{ExpiresAt: &iso}).ExpiresAtTime()
	if err != nil || got == nil || !got.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("ExpiresAtTime() = %v, %v", got, err)
	}
	for name, raw := range map[string]*string{"null": nil, "empty": new(string)} {
		got, err := (&APIKeyData{ExpiresAt: raw}).ExpiresAtTime()
		if err != nil || got != nil {
			t.Errorf("ExpiresAtTime() on a non-expiring key (%s) = %v, %v; want nil, nil", name, got, err)
		}
	}
	// A value that is present but unreadable must not pass as "never expires".
	bad := "01/01/2027"
	if got, err := (&APIKeyData{ExpiresAt: &bad}).ExpiresAtTime(); err == nil {
		t.Fatalf("ExpiresAtTime() on %q = %v, nil; want an error", bad, got)
	}
}
