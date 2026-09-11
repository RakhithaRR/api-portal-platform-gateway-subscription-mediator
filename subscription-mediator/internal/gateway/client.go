// Package gateway is a small client for the WSO2 API Platform Gateway 1.2.0
// Controller management REST API, as described by that release's
// management-openapi.yaml.
//
// Only the operations the mediator needs are modelled: listing REST APIs,
// subscription plans, subscriptions, and API keys. The request and response
// shapes are those of gateway 1.2.0 and are not guaranteed against any other
// release.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

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
	APIID              string `json:"apiId"`
	ApplicationID      string `json:"applicationId,omitempty"`
	SubscriptionPlanID string `json:"subscriptionPlanId,omitempty"`
	Status             string `json:"status"`
}

// SubscriptionCreateRequest is the body of POST /subscriptions.
type SubscriptionCreateRequest struct {
	APIID              string `json:"apiId"`
	SubscriptionToken  string `json:"subscriptionToken"`
	ApplicationID      string `json:"applicationId,omitempty"`
	SubscriptionPlanID string `json:"subscriptionPlanId,omitempty"`
	Status             string `json:"status,omitempty"`
}

// APIKeyRequest is the body of the API key create and update operations.
type APIKeyRequest struct {
	Name string `json:"name,omitempty"`
	// APIKey injects the portal's own key value; the gateway hashes it.
	APIKey        string     `json:"apiKey,omitempty"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
	ExternalRefID string     `json:"externalRefId,omitempty"`
}

// APIKey is a gateway API key.
type APIKey struct {
	Name   string `json:"name"`
	APIID  string `json:"apiId"`
	Status string `json:"status"`
}

// Client talks to the management API over HTTP basic auth.
type Client struct {
	baseURL  string
	username string
	password string
	http     *http.Client
}

// New builds a Client.
func New(baseURL, username, password string) *Client {
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		username: username,
		password: password,
		http:     &http.Client{Timeout: 15 * time.Second},
	}
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

// GetSubscriptionPlan fetches one plan by gateway id.
func (c *Client) GetSubscriptionPlan(ctx context.Context, planID string) (*SubscriptionPlan, error) {
	var plan SubscriptionPlan
	err := c.do(ctx, http.MethodGet, "/subscription-plans/"+url.PathEscape(planID), nil, nil, &plan)
	return &plan, err
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

// ListSubscriptions lists subscriptions, optionally filtered.
func (c *Client) ListSubscriptions(ctx context.Context, apiID, applicationID string) ([]Subscription, error) {
	query := url.Values{}
	if apiID != "" {
		query.Set("apiId", apiID)
	}
	if applicationID != "" {
		query.Set("applicationId", applicationID)
	}
	var resp struct {
		Subscriptions []Subscription `json:"subscriptions"`
	}
	if err := c.do(ctx, http.MethodGet, "/subscriptions", query, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Subscriptions, nil
}

// UpdateSubscriptionStatus changes a subscription's status. The gateway's update
// operation accepts nothing else — no plan change, no new token.
func (c *Client) UpdateSubscriptionStatus(ctx context.Context, subscriptionID, status string) error {
	body := map[string]string{"status": status}
	return c.do(ctx, http.MethodPut, "/subscriptions/"+url.PathEscape(subscriptionID), nil, body, nil)
}

// DeleteSubscription deletes a subscription.
func (c *Client) DeleteSubscription(ctx context.Context, subscriptionID string) error {
	return c.do(ctx, http.MethodDelete, "/subscriptions/"+url.PathEscape(subscriptionID), nil, nil, nil)
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
