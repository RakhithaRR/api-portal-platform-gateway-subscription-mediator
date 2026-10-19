// Package gateway is a small client for the WSO2 API Platform Gateway 1.2.0
// Controller management REST API, as described by that release's
// management-openapi.yaml.
//
// Only the operations the mediator needs are modelled: listing REST APIs,
// subscription plans, subscriptions, and API keys. The request and response
// shapes are those of gateway 1.2.0 and are not guaranteed against any other
// release.
//
// The subscription operations also rely on two changes the stock 1.2.0 API does
// not have (https://github.com/wso2/api-platform/issues/3782, merged as
// https://github.com/wso2/api-platform/pull/3804):
//
//   - POST /subscriptions accepts an externalRefId, which becomes the
//     subscription's ID, so GET/PUT/DELETE /subscriptions/{id} address it;
//   - PUT /subscriptions/{id} is a partial update that also accepts
//     subscriptionPlanId (null clears it) and subscriptionToken.
package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ExternalRefIDField is the POST /subscriptions field that sets a caller-chosen
// subscription ID (wso2/api-platform#3804). The gateway stores the subscription
// under that ID, so it is read back with GET /subscriptions/{id}.
const ExternalRefIDField = "externalRefId"

// Subscription statuses the gateway accepts.
const (
	StatusActive   = "ACTIVE"
	StatusInactive = "INACTIVE"
	StatusRevoked  = "REVOKED"
)

// APIError is a non-2xx response from the gateway.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gateway returned %d: %s", e.StatusCode, e.Message)
}

// IsNotFound reports a 404.
func (e *APIError) IsNotFound() bool { return e.StatusCode == http.StatusNotFound }

// IsConflict reports a 409 — typically "already subscribed".
func (e *APIError) IsConflict() bool { return e.StatusCode == http.StatusConflict }

// IsBadRequest reports a 400.
func (e *APIError) IsBadRequest() bool { return e.StatusCode == http.StatusBadRequest }

// AsAPIError extracts an *APIError if the error is one.
func AsAPIError(err error) (*APIError, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}

// RestAPI is one entry of GET /rest-apis. Only the fields the mediator matches
// on are modelled.
type RestAPI struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		DisplayName string `json:"displayName"`
		Version     string `json:"version"`
		// SubscriptionPlans is the API's own plan allowlist. When non-empty the
		// gateway rejects a subscription whose plan is not in it.
		SubscriptionPlans []string `json:"subscriptionPlans,omitempty"`
	} `json:"spec"`
	Status struct {
		ID string `json:"id"`
	} `json:"status"`
}

// Handle returns the identifier the gateway accepts wherever an apiId is wanted.
func (a *RestAPI) Handle() string {
	if a.Status.ID != "" {
		return a.Status.ID
	}
	return a.Metadata.Name
}

// SubscriptionPlan is a gateway subscription plan.
type SubscriptionPlan struct {
	ID       string `json:"id"`
	PlanName string `json:"planName"`
	Status   string `json:"status,omitempty"`
}

// Subscription is a gateway subscription.
type Subscription struct {
	ID                 string `json:"id"`
	ExternalRefID      string `json:"externalRefId,omitempty"`
	APIID              string `json:"apiId"`
	ApplicationID      string `json:"applicationId,omitempty"`
	SubscriptionPlanID string `json:"subscriptionPlanId,omitempty"`
	Status             string `json:"status"`
}

// SubscriptionCreateRequest is the body of POST /subscriptions.
type SubscriptionCreateRequest struct {
	// ExternalRefID is the caller's own identifier for the subscription. The
	// gateway uses it as the subscription ID instead of generating one, so every
	// later lookup, update and delete addresses the subscription by it. It must
	// match ^[A-Za-z0-9][A-Za-z0-9._-]*$ and be at most 64 characters.
	ExternalRefID      string `json:"externalRefId,omitempty"`
	APIID              string `json:"apiId"`
	SubscriptionToken  string `json:"subscriptionToken"`
	ApplicationID      string `json:"applicationId,omitempty"`
	SubscriptionPlanID string `json:"subscriptionPlanId,omitempty"`
	Status             string `json:"status,omitempty"`
}

// SubscriptionUpdate is a partial update for PUT /subscriptions/{id}: a nil
// field is left out of the body and stays as it is on the gateway.
type SubscriptionUpdate struct {
	Status            *string
	SubscriptionToken *string
	// SubscriptionPlanID set to "" clears the plan, sent as an explicit null.
	SubscriptionPlanID *string
}

// MarshalJSON sends only the fields that are set.
func (u SubscriptionUpdate) MarshalJSON() ([]byte, error) {
	body := map[string]any{}
	if u.Status != nil {
		body["status"] = *u.Status
	}
	if u.SubscriptionToken != nil {
		body["subscriptionToken"] = *u.SubscriptionToken
	}
	if u.SubscriptionPlanID != nil {
		if *u.SubscriptionPlanID == "" {
			body["subscriptionPlanId"] = nil
		} else {
			body["subscriptionPlanId"] = *u.SubscriptionPlanID
		}
	}
	return json.Marshal(body)
}

// APIKeyRequest is the body of the API key create and update operations.
type APIKeyRequest struct {
	Name string `json:"name,omitempty"`
	// APIKey injects the portal's own key value; the gateway hashes it.
	APIKey        string     `json:"apiKey,omitempty"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
	ExternalRefID string     `json:"externalRefId,omitempty"`
}

// API key statuses the gateway reports.
const KeyStatusRevoked = "revoked"

// APIKey is a gateway API key.
type APIKey struct {
	Name          string `json:"name"`
	APIID         string `json:"apiId"`
	Status        string `json:"status"`
	ExternalRefID string `json:"externalRefId,omitempty"`
}

// Client talks to the management API over HTTP basic auth.
type Client struct {
	baseURL  string
	username string
	password string
	http     *http.Client
}

// New builds a Client. caFile, when set, names a PEM bundle of CA certificates
// trusted for the gateway's TLS listener in addition to the system roots.
func New(baseURL, username, password, caFile string) (*Client, error) {
	tlsConfig, err := clientTLSConfig(caFile)
	if err != nil {
		return nil, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		username: username,
		password: password,
		http:     &http.Client{Timeout: 15 * time.Second, Transport: transport},
	}, nil
}

// clientTLSConfig trusts the system roots plus the certificates in caFile.
func clientTLSConfig(caFile string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read the gateway CA file: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil || roots == nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("the gateway CA file %s holds no PEM certificate", caFile)
	}
	cfg.RootCAs = roots
	return cfg, nil
}

// do performs one request and decodes the response into out when non-nil.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to encode the request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("gateway %s %s failed: %w", method, path, err)
	}
	defer resp.Body.Close()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read the gateway response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		apiErr := &APIError{StatusCode: resp.StatusCode, Message: http.StatusText(resp.StatusCode)}
		var decoded struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(payload, &decoded) == nil && decoded.Message != "" {
			apiErr.Message = decoded.Message
		}
		return apiErr
	}

	if out == nil || len(payload) == 0 {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("failed to decode the gateway response: %w", err)
	}
	return nil
}

// ListRestAPIs lists REST APIs, optionally filtered by display name and version.
func (c *Client) ListRestAPIs(ctx context.Context, displayName, version string) ([]RestAPI, error) {
	query := url.Values{}
	if displayName != "" {
		query.Set("displayName", displayName)
	}
	if version != "" {
		query.Set("version", version)
	}
	var resp struct {
		APIs []RestAPI `json:"apis"`
	}
	if err := c.do(ctx, http.MethodGet, "/rest-apis", query, nil, &resp); err != nil {
		return nil, err
	}
	return resp.APIs, nil
}

// ListSubscriptionPlans lists every subscription plan.
func (c *Client) ListSubscriptionPlans(ctx context.Context) ([]SubscriptionPlan, error) {
	var resp struct {
		SubscriptionPlans []SubscriptionPlan `json:"subscriptionPlans"`
	}
	if err := c.do(ctx, http.MethodGet, "/subscription-plans", nil, nil, &resp); err != nil {
		return nil, err
	}
	return resp.SubscriptionPlans, nil
}

// CreateSubscriptionPlan creates a plan.
func (c *Client) CreateSubscriptionPlan(ctx context.Context, planName string) (*SubscriptionPlan, error) {
	body := map[string]string{"planName": planName, "status": "ACTIVE"}
	var plan SubscriptionPlan
	err := c.do(ctx, http.MethodPost, "/subscription-plans", nil, body, &plan)
	return &plan, err
}

// CreateSubscription creates a subscription.
func (c *Client) CreateSubscription(ctx context.Context, req *SubscriptionCreateRequest) (*Subscription, error) {
	var sub Subscription
	err := c.do(ctx, http.MethodPost, "/subscriptions", nil, req, &sub)
	return &sub, err
}

// FindSubscription returns the subscription created with externalRefId, or nil
// when the gateway has none. The externalRefId is the subscription's ID.
func (c *Client) FindSubscription(ctx context.Context, externalRefID string) (*Subscription, error) {
	var sub Subscription
	err := c.do(ctx, http.MethodGet, "/subscriptions/"+url.PathEscape(externalRefID), nil, nil, &sub)
	if apiErr, ok := AsAPIError(err); ok && apiErr.IsNotFound() {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Checked again so a gateway without caller-set IDs (one that predates
	// wso2/api-platform#3804) can't hand back somebody else's subscription.
	if sub.ID != externalRefID {
		return nil, fmt.Errorf("the gateway returned subscription %q for ID %q; it may not support %s",
			sub.ID, externalRefID, ExternalRefIDField)
	}
	return &sub, nil
}

// UpdateSubscription applies a partial update.
func (c *Client) UpdateSubscription(ctx context.Context, subscriptionID string, update SubscriptionUpdate) error {
	return c.do(ctx, http.MethodPut, "/subscriptions/"+url.PathEscape(subscriptionID), nil, update, nil)
}

// DeleteSubscription deletes a subscription.
func (c *Client) DeleteSubscription(ctx context.Context, subscriptionID string) error {
	return c.do(ctx, http.MethodDelete, "/subscriptions/"+url.PathEscape(subscriptionID), nil, nil, nil)
}

// ListAPIKeys lists every key on a REST API, revoked ones included.
func (c *Client) ListAPIKeys(ctx context.Context, apiHandle string) ([]APIKey, error) {
	var resp struct {
		APIKeys []APIKey `json:"apiKeys"`
	}
	path := "/rest-apis/" + url.PathEscape(apiHandle) + "/api-keys"
	if err := c.do(ctx, http.MethodGet, path, nil, nil, &resp); err != nil {
		return nil, err
	}
	return resp.APIKeys, nil
}

// CreateAPIKey creates an API key for a REST API, injecting the portal's value.
func (c *Client) CreateAPIKey(ctx context.Context, apiHandle string, req *APIKeyRequest) (*APIKey, error) {
	var resp struct {
		APIKey *APIKey `json:"apiKey"`
	}
	path := "/rest-apis/" + url.PathEscape(apiHandle) + "/api-keys"
	if err := c.do(ctx, http.MethodPost, path, nil, req, &resp); err != nil {
		return nil, err
	}
	if resp.APIKey == nil {
		return nil, fmt.Errorf("the gateway accepted the API key but returned no key record")
	}
	return resp.APIKey, nil
}

// UpdateAPIKey replaces an existing key's value, which is how a portal-side
// regeneration is applied.
func (c *Client) UpdateAPIKey(ctx context.Context, apiHandle, keyName string, req *APIKeyRequest) (*APIKey, error) {
	var resp struct {
		APIKey *APIKey `json:"apiKey"`
	}
	path := "/rest-apis/" + url.PathEscape(apiHandle) + "/api-keys/" + url.PathEscape(keyName)
	if err := c.do(ctx, http.MethodPut, path, nil, req, &resp); err != nil {
		return nil, err
	}
	if resp.APIKey == nil {
		return nil, fmt.Errorf("the gateway accepted the API key update but returned no key record")
	}
	return resp.APIKey, nil
}

// RevokeAPIKey revokes an API key.
func (c *Client) RevokeAPIKey(ctx context.Context, apiHandle, keyName string) error {
	path := "/rest-apis/" + url.PathEscape(apiHandle) + "/api-keys/" + url.PathEscape(keyName)
	return c.do(ctx, http.MethodDelete, path, nil, nil, nil)
}

// Ping checks reachability and credentials with the cheapest authenticated read.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.ListSubscriptionPlans(ctx)
	return err
}
