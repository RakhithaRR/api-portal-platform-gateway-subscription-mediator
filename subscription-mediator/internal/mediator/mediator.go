// Package mediator translates WSO2 API Portal webhook events into Gateway
// Controller management REST API calls.
//
// The flow is: the HTTP layer verifies a delivery and durably queues it; a
// dispatcher hands queued events to a fixed set of ordered lanes; each lane
// applies its events to the gateway, retrying transient failures with bounded
// backoff.
//
// Durable queueing before acknowledgement is the design constraint. The API
// Portal attempts each delivery exactly once, with no retry and no dead-letter
// queue, so an event that is accepted and then lost — to a restart, or to a
// gateway that happens to be down — is gone for good.
package mediator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"subscription-mediator/internal/gateway"
	"subscription-mediator/internal/store"
	"subscription-mediator/internal/webhook"
)

// Tuning. These were once configurable; a demo has no reason to change them, so
// they are constants and the configuration stays small.
const (
	// Workers is the number of ordered apply lanes. Events for one subscription
	// always land on the same lane, so they apply in the order received.
	Workers = 4
	// LaneCapacity bounds each lane's in-memory channel.
	LaneCapacity = 64
	// MaxPendingEvents caps the durable pending queue. Beyond it the webhook
	// endpoint rejects a delivery rather than growing without limit.
	MaxPendingEvents = 5000
	// MaxAttempts is the retry budget for a transient failure.
	MaxAttempts = 6
	// RetryBaseDelay and RetryMaxDelay bound the exponential backoff.
	RetryBaseDelay = time.Second
	RetryMaxDelay  = 2 * time.Minute
	// ScanInterval is how often the dispatcher rescans the durable queue for
	// records whose backoff has elapsed.
	ScanInterval = 15 * time.Second
	// APICacheTTL bounds how long a resolved API mapping is reused.
	APICacheTTL = 5 * time.Minute
)

// Mediator owns the queue, the worker lanes, and the translation logic.
type Mediator struct {
	client   *gateway.Client
	store    *store.Store
	codec    *webhook.Codec
	resolver *resolver
	logger   *slog.Logger

	// lanes are the ordered apply queues. An event is routed to a lane by a hash
	// of its ordering key, so every event touching one subscription (or one API
	// key) lands on the same lane and is applied in the order received.
	lanes    []chan uint64
	inflight sync.Map
	notify   chan struct{}
	wg       sync.WaitGroup
	started  atomic.Bool

	applied    atomic.Int64
	failedPerm atomic.Int64
}

// New builds a Mediator.
func New(client *gateway.Client, st *store.Store, codec *webhook.Codec, apiHandles map[string]string, logger *slog.Logger) *Mediator {
	m := &Mediator{
		client: client,
		store:  st,
		codec:  codec,
		logger: logger,
		notify: make(chan struct{}, 1),
	}
	m.resolver = newResolver(client, st, apiHandles, logger)
	m.lanes = make([]chan uint64, Workers)
	for i := range m.lanes {
		m.lanes[i] = make(chan uint64, LaneCapacity)
	}
	return m
}

// Accept durably queues a verified delivery and returns once it is on disk.
//
// It returns store.ErrQueueFull when the pending queue is at its ceiling, which
// the HTTP layer turns into a 503 — the queue in front of the bounded worker
// pool is itself bounded, so a gateway outage cannot grow it without limit.
func (m *Mediator) Accept(env *webhook.Envelope, body []byte) (queued bool, err error) {
	if m.store.AlreadyProcessed(env.EventID) {
		return false, nil
	}
	record, err := m.store.Enqueue(&store.Pending{
		EventID:     env.EventID,
		EventType:   env.EventType,
		OrderingKey: orderingKey(env, body),
		Body:        body,
		ReceivedAt:  time.Now().UTC(),
	}, MaxPendingEvents)
	if err != nil {
		return false, err
	}
	m.dispatch(record)
	return true, nil
}

// Start launches the worker lanes and the dispatcher, and replays whatever the
// queue held from a previous run.
func (m *Mediator) Start(ctx context.Context) {
	if m.started.Swap(true) {
		return
	}
	for i := range m.lanes {
		lane := i
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.runLane(ctx, lane)
		}()
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.runDispatcher(ctx)
	}()
}

// Wait blocks until every worker has stopped. The caller cancels the context first.
func (m *Mediator) Wait() { m.wg.Wait() }

// Stats reports counters for the status endpoint.
func (m *Mediator) Stats() (applied, permanentFailures int64) {
	return m.applied.Load(), m.failedPerm.Load()
}

// Nudge asks the dispatcher to rescan the durable queue now, so a requeued
// failure does not wait out the scan interval.
func (m *Mediator) Nudge() {
	select {
	case m.notify <- struct{}{}:
	default:
	}
}

// runDispatcher moves due queue records onto lanes.
//
// It wakes on a new arrival and on a jittered interval. The jitter is computed
// and waited before each scan, not after, so several mediator instances
// restarting at once do not all hit the gateway on their very first pass.
func (m *Mediator) runDispatcher(ctx context.Context) {
	// The queue survived the last shutdown; replay it before waiting on anything.
	m.scan(ctx)

	timer := time.NewTimer(m.nextWake())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.notify:
			m.scan(ctx)
		case <-timer.C:
			m.scan(ctx)
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(m.nextWake())
	}
}

// nextWake is how long the dispatcher sleeps before its next scan: the jittered
// idle interval, or sooner if a retry's backoff elapses before then. Without
// that second case a one-second backoff would still wait out the whole idle
// interval, making the backoff schedule meaningless.
func (m *Mediator) nextWake() time.Duration {
	wake := jitteredInterval()
	if due, ok := m.store.NextDue(time.Now()); ok {
		if until := time.Until(due); until < wake {
			wake = until
		}
	}
	if wake < 50*time.Millisecond {
		wake = 50 * time.Millisecond
	}
	return wake
}

// jitteredInterval spreads the rescan so instances desynchronize.
func jitteredInterval() time.Duration {
	interval := ScanInterval
	if half := interval / 2; half > 0 {
		interval += rand.N(half)
	}
	return interval
}

// scan dispatches every due record, oldest first.
func (m *Mediator) scan(ctx context.Context) {
	// One scan takes at most the total lane capacity, so a large backlog drains
	// over several passes rather than being held in memory all at once.
	for _, record := range m.store.DuePending(time.Now(), Workers*LaneCapacity) {
		if ctx.Err() != nil {
			return
		}
		m.dispatch(record)
	}
}

// dispatch hands one record to its lane. The record stays on disk either way:
// if the lane is full the next scan picks it up, so nothing is dropped.
func (m *Mediator) dispatch(record *store.Pending) {
	if _, loaded := m.inflight.LoadOrStore(record.Seq, struct{}{}); loaded {
		return
	}
	lane := m.lanes[laneFor(record.OrderingKey, len(m.lanes))]
	select {
	case lane <- record.Seq:
	default:
		// Lane saturated. Release the marker so a later scan retries the handoff.
		m.inflight.Delete(record.Seq)
	}
}

// runLane applies the events routed to one lane, in order.
func (m *Mediator) runLane(ctx context.Context, lane int) {
	for {
		select {
		case <-ctx.Done():
			return
		case seq := <-m.lanes[lane]:
			m.applySequence(ctx, seq)
			m.inflight.Delete(seq)
		}
	}
}

// applySequence applies one queued record and records the outcome.
func (m *Mediator) applySequence(ctx context.Context, seq uint64) {
	record, ok := m.store.Pending(seq)
	if !ok {
		return // already completed
	}
	if record.State != store.StateReady || record.NextAttempt.After(time.Now()) {
		return // retired, or its backoff was extended after dispatch
	}

	log := m.logger.With(
		slog.String("event_id", record.EventID),
		slog.String("event_type", record.EventType),
		slog.Uint64("queue_seq", record.Seq),
		slog.Int("attempt", record.Attempts+1),
	)

	if m.store.AlreadyProcessed(record.EventID) {
		if err := m.store.CompletePending(record.Seq); err != nil {
			log.Warn("failed to dequeue an already-processed event", slog.Any("error", err))
		}
		return
	}

	err := m.apply(ctx, record, log)
	switch {
	case err == nil:
		if markErr := m.store.MarkProcessed(record.EventID); markErr != nil {
			log.Error("applied the event but failed to record it as processed", slog.Any("error", markErr))
		}
		if completeErr := m.store.CompletePending(record.Seq); completeErr != nil {
			log.Error("applied the event but failed to dequeue it", slog.Any("error", completeErr))
		}
		m.applied.Add(1)
		m.Nudge()

	case errors.Is(err, context.Canceled):
		// Shutting down. Leave the record queued exactly as it is so the next run
		// picks it up without consuming an attempt.
		log.Info("apply interrupted by shutdown; event stays queued")

	case isPermanent(err), record.Attempts+1 >= MaxAttempts:
		record.Attempts++
		record.State = store.StateFailed
		record.LastError = err.Error()
		if updateErr := m.store.UpdatePending(record); updateErr != nil {
			log.Error("failed to record a permanently failed event", slog.Any("error", updateErr))
		}
		m.failedPerm.Add(1)
		if isPermanent(err) {
			log.Error("event cannot be applied and will not be retried", slog.Any("error", err))
		} else {
			log.Error("event failed after exhausting every attempt",
				slog.Int("attempts", record.Attempts), slog.Any("error", err))
		}

	default:
		record.Attempts++
		record.LastError = err.Error()
		record.NextAttempt = time.Now().Add(backoff(record.Attempts))
		if updateErr := m.store.UpdatePending(record); updateErr != nil {
			log.Error("failed to record a retry deadline", slog.Any("error", updateErr))
		}
		log.Warn("event apply failed; will retry",
			slog.Duration("retry_in", time.Until(record.NextAttempt)),
			slog.Any("error", err))
		// Wake the dispatcher so it recomputes its sleep against this new
		// deadline. Its timer was armed before this failure existed, so without
		// the nudge the retry would wait out the whole idle interval.
		m.Nudge()
	}
}

// apply routes one event to its handler.
func (m *Mediator) apply(ctx context.Context, record *store.Pending, log *slog.Logger) error {
	env, err := webhook.Decode(record.Body)
	if err != nil {
		return permanentf("%v", err)
	}

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

// backoff returns the delay before attempt n, doubling from the base delay and
// capped at the maximum, with jitter so simultaneous failures do not retry in
// lockstep.
func backoff(attempt int) time.Duration {
	delay := RetryBaseDelay
	for i := 1; i < attempt && delay < RetryMaxDelay; i++ {
		delay *= 2
	}
	if delay > RetryMaxDelay {
		delay = RetryMaxDelay
	}
	if half := delay / 2; half > 0 {
		delay = delay/2 + rand.N(half+1)
	}
	return delay
}

// orderingKey groups events that must not be reordered relative to one another.
//
// Every event for one subscription shares a key, as does every event for one API
// key, so a create and the delete that follows it can never be applied out of
// order. Events for unrelated subscriptions are free to run concurrently.
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

// laneFor picks the lane for an ordering key.
func laneFor(key string, lanes int) int {
	if lanes <= 1 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(lanes))
}

// permanentError marks a failure that no retry can fix — a malformed event, an
// unsupported artifact kind, an API the gateway does not have, a request the
// gateway rejected as invalid.
type permanentError struct{ msg string }

func (e *permanentError) Error() string { return e.msg }

func permanentf(format string, args ...any) error {
	return &permanentError{msg: fmt.Sprintf(format, args...)}
}

// isPermanent reports whether an error should not be retried.
func isPermanent(err error) bool {
	if err == nil {
		return false
	}
	var pe *permanentError
	return errors.As(err, &pe)
}
