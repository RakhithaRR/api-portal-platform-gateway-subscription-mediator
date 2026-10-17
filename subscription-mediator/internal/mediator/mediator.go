// Package mediator translates WSO2 API Portal webhook events into Gateway
// Controller management REST API calls.
//
// Each event is applied to the gateway while the portal's delivery waits, and
// the HTTP layer answers with the outcome. Nothing is stored: every lookup
// reads the gateway. A subscription is found by the portal's subscription ID,
// which the gateway subscription is created under (its externalRefId), and an API key by
// the portal's key ID, carried as its externalRefId.
//
// The API Portal attempts each delivery exactly once, with no retry. A failure
// here is reported back to the portal, which marks the delivery FAILED in its
// own delivery history; repeating the action in the portal sends a new event.
package mediator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"subscription-mediator/internal/gateway"
	"subscription-mediator/internal/webhook"
)

// APICacheTTL bounds how long a resolved API mapping is reused.
const APICacheTTL = 5 * time.Minute

// Mediator owns the translation logic.
type Mediator struct {
	client   *gateway.Client
	codec    *webhook.Codec
	resolver *resolver
	logger   *slog.Logger

	// locks serializes events that share an ordering key, so two deliveries
	// for one subscription arriving together are applied one after the other.
	locks keyedMutex

	applied atomic.Int64
	failed  atomic.Int64
}

// New builds a Mediator.
func New(client *gateway.Client, codec *webhook.Codec, apiHandles map[string]string, logger *slog.Logger) *Mediator {
	return &Mediator{
		client:   client,
		codec:    codec,
		resolver: newResolver(client, apiHandles, logger),
		logger:   logger,
	}
}

// Handle applies one verified event to the gateway and returns once it is
// done. An error that IsPermanent reports no retry can fix; any other error is
// a gateway the mediator could not reach or that failed on its side.
func (m *Mediator) Handle(ctx context.Context, env *webhook.Envelope, body []byte) error {
	unlock := m.locks.lock(orderingKey(env, body))
	defer unlock()

	log := m.logger.With(
		slog.String("event_id", env.EventID),
		slog.String("event_type", env.EventType),
	)
	if err := m.apply(ctx, env, log); err != nil {
		m.failed.Add(1)
		return err
	}
	m.applied.Add(1)
	return nil
}

// Stats reports how many events were applied and how many failed since start.
func (m *Mediator) Stats() (applied, failed int64) {
	return m.applied.Load(), m.failed.Load()
}

// apply routes one event to its handler.
func (m *Mediator) apply(ctx context.Context, env *webhook.Envelope, log *slog.Logger) error {
	switch env.EventType {
	case webhook.EventSubscriptionCreated:
		return m.handleSubscriptionCreated(ctx, env, log)
	case webhook.EventSubscriptionUpdated:
		return m.handleSubscriptionUpdated(ctx, env, log)
	case webhook.EventSubscriptionPlanChanged:
		return m.handleSubscriptionPlanChanged(ctx, env, log)
	case webhook.EventSubscriptionTokenRegenerated:
		return m.handleSubscriptionTokenRegenerated(ctx, env, log)
	case webhook.EventSubscriptionDeleted:
		return m.handleSubscriptionDeleted(ctx, env, log)

	case webhook.EventAPIKeyGenerated:
		return m.handleAPIKeyGenerated(ctx, env, log)
	case webhook.EventAPIKeyRegenerated:
		return m.handleAPIKeyRegenerated(ctx, env, log)
	case webhook.EventAPIKeyRevoked:
		return m.handleAPIKeyRevoked(ctx, env, log)

	default:
		// An application.* event, or a newer portal event this build does not
		// translate. The gateway has no application resource — an application
		// appears there only as the opaque applicationId on a subscription — so
		// there is nothing to do, and acknowledging is correct.
		log.Info("no gateway operation is defined for this event type; acknowledging it")
		return nil
	}
}

// orderingKey groups events that must not be applied concurrently.
//
// Every event for one subscription shares a key, as does every event for one API
// key. Events for unrelated subscriptions are free to run concurrently.
func orderingKey(env *webhook.Envelope, body []byte) string {
	var probe struct {
		Data struct {
			SubscriptionID string `json:"subscription_id"`
			KeyID          string `json:"key_id"`
			ApplicationID  string `json:"application_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &probe); err == nil {
		switch {
		case probe.Data.SubscriptionID != "":
			return "sub:" + probe.Data.SubscriptionID
		case probe.Data.KeyID != "":
			return "key:" + probe.Data.KeyID
		case probe.Data.ApplicationID != "":
			return "app:" + probe.Data.ApplicationID
		}
	}
	return "event:" + env.EventID
}

// keyedMutex hands out one lock per key, and forgets a key once nobody holds
// or waits on it, so the map does not grow with every subscription ever seen.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyLock
}

type keyLock struct {
	mu   sync.Mutex
	refs int
}

// lock blocks until key is free and returns the function that releases it.
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyLock{}
	}
	l, ok := k.locks[key]
	if !ok {
		l = &keyLock{}
		k.locks[key] = l
	}
	l.refs++
	k.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		k.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

// permanentError marks a failure that no retry can fix — a malformed event, an
// unsupported artifact kind, an API the gateway does not have, a request the
// gateway rejected as invalid.
type permanentError struct{ msg string }

func (e *permanentError) Error() string { return e.msg }

func permanentf(format string, args ...any) error {
	return &permanentError{msg: fmt.Sprintf(format, args...)}
}

// IsPermanent reports whether an error is one no retry can fix.
func IsPermanent(err error) bool {
	if err == nil {
		return false
	}
	var pe *permanentError
	return errors.As(err, &pe)
}
