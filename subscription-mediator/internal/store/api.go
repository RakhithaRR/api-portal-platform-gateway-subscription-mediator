package store

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// --- identifier mappings ---------------------------------------------------

// APIMapping returns the gateway API handle previously resolved for a portal reference.
func (s *Store) APIMapping(portalRef string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	handle, ok := s.apiMappings[portalRef]
	return handle, ok
}

// PutAPIMapping records that a portal API reference resolves to a gateway handle.
func (s *Store) PutAPIMapping(portalRef, gatewayHandle string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, err := marshalValue(gatewayHandle)
	if err != nil {
		return err
	}
	if err := s.appendLocked(&entry{Kind: kindAPIMapping, Key: portalRef, Value: value}); err != nil {
		return err
	}
	s.apiMappings[portalRef] = gatewayHandle
	return nil
}

// PlanMapping returns the gateway plan ID previously resolved for a portal plan.
func (s *Store) PlanMapping(portalPlanRef string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	planID, ok := s.planMappings[portalPlanRef]
	return planID, ok
}

// PutPlanMapping records that a portal plan reference resolves to a gateway plan.
func (s *Store) PutPlanMapping(portalPlanRef, gatewayPlanID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, err := marshalValue(gatewayPlanID)
	if err != nil {
		return err
	}
	if err := s.appendLocked(&entry{Kind: kindPlanMapping, Key: portalPlanRef, Value: value}); err != nil {
		return err
	}
	s.planMappings[portalPlanRef] = gatewayPlanID
	return nil
}

// DeletePlanMapping forgets a plan mapping, for when the gateway no longer has it.
func (s *Store) DeletePlanMapping(portalPlanRef string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.planMappings[portalPlanRef]; !ok {
		return nil
	}
	if err := s.appendLocked(&entry{Kind: kindPlanMapping, Key: portalPlanRef, Deleted: true}); err != nil {
		return err
	}
	delete(s.planMappings, portalPlanRef)
	return nil
}

// --- subscriptions ---------------------------------------------------------

// Subscription returns the mapping for a portal subscription ID.
func (s *Store) Subscription(portalSubscriptionID string) (*Subscription, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sub, ok := s.subs[portalSubscriptionID]
	if !ok {
		return nil, false
	}
	clone := *sub
	return &clone, true
}

// PutSubscription records or replaces the mapping for one portal subscription.
func (s *Store) PutSubscription(sub *Subscription) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := *sub
	clone.UpdatedAt = time.Now().UTC()
	value, err := marshalValue(&clone)
	if err != nil {
		return err
	}
	if err := s.appendLocked(&entry{Kind: kindSubscription, Key: clone.PortalSubscriptionID, Value: value}); err != nil {
		return err
	}
	s.subs[clone.PortalSubscriptionID] = &clone
	return nil
}

// DeleteSubscription forgets a subscription mapping.
func (s *Store) DeleteSubscription(portalSubscriptionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.subs[portalSubscriptionID]; !ok {
		return nil
	}
	if err := s.appendLocked(&entry{Kind: kindSubscription, Key: portalSubscriptionID, Deleted: true}); err != nil {
		return err
	}
	delete(s.subs, portalSubscriptionID)
	return nil
}

// SubscriptionCount reports how many subscription mappings are held.
func (s *Store) SubscriptionCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.subs)
}

// --- API keys --------------------------------------------------------------

// APIKeyByPortalID returns the mapping for a portal API key ID.
func (s *Store) APIKeyByPortalID(portalKeyID string) (*APIKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keys[portalKeyID]
	if !ok {
		return nil, false
	}
	clone := *k
	return &clone, true
}

// PutAPIKey records or replaces the mapping for one portal API key.
func (s *Store) PutAPIKey(key *APIKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := *key
	clone.UpdatedAt = time.Now().UTC()
	value, err := marshalValue(&clone)
	if err != nil {
		return err
	}
	if err := s.appendLocked(&entry{Kind: kindAPIKey, Key: clone.PortalKeyID, Value: value}); err != nil {
		return err
	}
	s.keys[clone.PortalKeyID] = &clone
	return nil
}

// DeleteAPIKey forgets an API key mapping.
func (s *Store) DeleteAPIKey(portalKeyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.keys[portalKeyID]; !ok {
		return nil
	}
	if err := s.appendLocked(&entry{Kind: kindAPIKey, Key: portalKeyID, Deleted: true}); err != nil {
		return err
	}
	delete(s.keys, portalKeyID)
	return nil
}

// GatewayKeyNameTaken reports whether a gateway key name is already mapped for
// an API by a different portal key, so a slugified name collision can be
// resolved instead of silently overwriting the other key.
func (s *Store) GatewayKeyNameTaken(gatewayAPIHandle, gatewayKeyName, exceptPortalKeyID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, k := range s.keys {
		if id == exceptPortalKeyID {
			continue
		}
		if k.GatewayAPIHandle == gatewayAPIHandle && k.GatewayKeyName == gatewayKeyName {
			return true
		}
	}
	return false
}

// APIKeyCount reports how many API key mappings are held.
func (s *Store) APIKeyCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.keys)
}

// --- processed-event ledger ------------------------------------------------

// AlreadyProcessed reports whether this event id has already been applied.
func (s *Store) AlreadyProcessed(eventID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.processed[eventID]
	return ok
}

// MarkProcessed records an event id as applied.
func (s *Store) MarkProcessed(eventID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	if err := s.appendLocked(&entry{Kind: kindProcessed, Key: eventID, Timestamp: now}); err != nil {
		return err
	}
	s.processed[eventID] = now
	return nil
}

// ProcessedCount reports how many event ids are remembered.
func (s *Store) ProcessedCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.processed)
}

// --- pending queue ---------------------------------------------------------

// Enqueue durably appends an event to the pending queue. maxPending bounds the
// number of READY records; beyond it ErrQueueFull is returned so the caller can
// reject the delivery rather than let the queue grow without limit.
func (s *Store) Enqueue(p *Pending, maxPending int) (*Pending, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ready := 0
	for _, existing := range s.pending {
		if existing.State == StateReady {
			ready++
		}
	}
	if maxPending > 0 && ready >= maxPending {
		return nil, ErrQueueFull
	}

	s.seq++
	clone := *p
	clone.Seq = s.seq
	clone.State = StateReady
	if clone.ReceivedAt.IsZero() {
		clone.ReceivedAt = time.Now().UTC()
	}
	value, err := marshalValue(&clone)
	if err != nil {
		s.seq--
		return nil, err
	}
	if err := s.appendLocked(&entry{Kind: kindPending, Key: clone.EventID, Value: value}); err != nil {
		s.seq--
		return nil, err
	}
	s.pending[clone.Seq] = &clone
	out := clone
	return &out, nil
}

// Pending returns one queued record by sequence number.
func (s *Store) Pending(seq uint64) (*Pending, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.pending[seq]
	if !ok {
		return nil, false
	}
	clone := *p
	return &clone, true
}

// UpdatePending rewrites a queued record — a new attempt count, backoff
// deadline, or terminal FAILED state.
func (s *Store) UpdatePending(p *Pending) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending[p.Seq]; !ok {
		return nil
	}
	clone := *p
	value, err := marshalValue(&clone)
	if err != nil {
		return err
	}
	if err := s.appendLocked(&entry{Kind: kindPending, Key: clone.EventID, Value: value}); err != nil {
		return err
	}
	s.pending[clone.Seq] = &clone
	return nil
}

// CompletePending removes a queued record after it has been applied.
func (s *Store) CompletePending(seq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.pending[seq]; !ok {
		return nil
	}
	value, err := marshalValue(seq)
	if err != nil {
		return err
	}
	if err := s.appendLocked(&entry{Kind: kindPendingDone, Key: fmt.Sprintf("%d", seq), Value: value}); err != nil {
		return err
	}
	delete(s.pending, seq)
	return nil
}

// DuePending returns READY records whose backoff has elapsed, oldest first,
// capped at limit. Sequence order is delivery order, which is what preserves
// per-key ordering downstream.
func (s *Store) DuePending(now time.Time, limit int) []*Pending {
	s.mu.RLock()
	defer s.mu.RUnlock()

	due := make([]*Pending, 0, len(s.pending))
	for _, p := range s.pending {
		if p.State != StateReady || p.NextAttempt.After(now) {
			continue
		}
		clone := *p
		due = append(due, &clone)
	}
	sort.Slice(due, func(i, j int) bool { return due[i].Seq < due[j].Seq })
	if limit > 0 && len(due) > limit {
		due = due[:limit]
	}
	return due
}

// NextDue returns the earliest NextAttempt among READY records that are not yet
// due, so the dispatcher can sleep exactly until a retry is ready instead of
// waiting out a fixed poll interval and making the backoff meaningless.
func (s *Store) NextDue(now time.Time) (time.Time, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var earliest time.Time
	for _, p := range s.pending {
		if p.State != StateReady || !p.NextAttempt.After(now) {
			continue
		}
		if earliest.IsZero() || p.NextAttempt.Before(earliest) {
			earliest = p.NextAttempt
		}
	}
	return earliest, !earliest.IsZero()
}

// PendingStats reports queue depth by state.
func (s *Store) PendingStats() (ready, failed int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.pending {
		switch p.State {
		case StateReady:
			ready++
		case StateFailed:
			failed++
		}
	}
	return ready, failed
}

// FailedPending returns the terminally failed records, oldest first, for an
// operator to inspect. Bodies are omitted — they are large and of no use here.
func (s *Store) FailedPending(limit int) []*Pending {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Pending, 0)
	for _, p := range s.pending {
		if p.State != StateFailed {
			continue
		}
		clone := *p
		clone.Body = nil
		out = append(out, &clone)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// RequeueFailed moves every FAILED record back to READY with its attempt count
// reset, so an operator can retry after fixing whatever blocked them.
func (s *Store) RequeueFailed() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	seqs := make([]uint64, 0)
	for seq, p := range s.pending {
		if p.State == StateFailed {
			seqs = append(seqs, seq)
		}
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })

	requeued := 0
	for _, seq := range seqs {
		clone := *s.pending[seq]
		clone.State = StateReady
		clone.Attempts = 0
		clone.NextAttempt = time.Time{}
		clone.LastError = ""
		value, err := marshalValue(&clone)
		if err != nil {
			return requeued, err
		}
		if err := s.appendLocked(&entry{Kind: kindPending, Key: clone.EventID, Value: value}); err != nil {
			return requeued, err
		}
		s.pending[seq] = &clone
		requeued++
	}
	return requeued, nil
}

// --- compaction ------------------------------------------------------------

// compactLocked rewrites the log as one entry per live record. The new file is
// written alongside and renamed into place, so a crash mid-compaction leaves the
// previous complete log intact.
func (s *Store) compactLocked() error {
	if err := s.writer.Flush(); err != nil {
		return err
	}

	tmpPath := s.path + ".compact"
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("failed to create the compaction file: %w", err)
	}
	w := bufio.NewWriter(tmp)

	fail := func(err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("state compaction failed: %w", err)
	}
	write := func(e *entry) error {
		if e.Timestamp.IsZero() {
			e.Timestamp = time.Now().UTC()
		}
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		_, err = w.Write(append(line, '\n'))
		return err
	}

	for key, handle := range s.apiMappings {
		value, err := marshalValue(handle)
		if err != nil {
			return fail(err)
		}
		if err := write(&entry{Kind: kindAPIMapping, Key: key, Value: value}); err != nil {
			return fail(err)
		}
	}
	for key, planID := range s.planMappings {
		value, err := marshalValue(planID)
		if err != nil {
			return fail(err)
		}
		if err := write(&entry{Kind: kindPlanMapping, Key: key, Value: value}); err != nil {
			return fail(err)
		}
	}
	for key, sub := range s.subs {
		value, err := marshalValue(sub)
		if err != nil {
			return fail(err)
		}
		if err := write(&entry{Kind: kindSubscription, Key: key, Value: value}); err != nil {
			return fail(err)
		}
	}
	for key, k := range s.keys {
		value, err := marshalValue(k)
		if err != nil {
			return fail(err)
		}
		if err := write(&entry{Kind: kindAPIKey, Key: key, Value: value}); err != nil {
			return fail(err)
		}
	}
	for id, ts := range s.processed {
		if err := write(&entry{Kind: kindProcessed, Key: id, Timestamp: ts}); err != nil {
			return fail(err)
		}
	}
	// Pending records are rewritten in sequence order so replay restores the
	// ordering the queue had before compaction.
	seqs := make([]uint64, 0, len(s.pending))
	for seq := range s.pending {
		seqs = append(seqs, seq)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for _, seq := range seqs {
		value, err := marshalValue(s.pending[seq])
		if err != nil {
			return fail(err)
		}
		if err := write(&entry{Kind: kindPending, Key: s.pending[seq].EventID, Value: value}); err != nil {
			return fail(err)
		}
	}
	// The highest sequence number must survive even when its record has already
	// completed, or a restart would reissue sequence numbers.
	if len(s.pending) == 0 && s.seq > 0 {
		value, err := marshalValue(s.seq)
		if err != nil {
			return fail(err)
		}
		if err := write(&entry{Kind: kindPendingDone, Key: fmt.Sprintf("%d", s.seq), Value: value}); err != nil {
			return fail(err)
		}
	}

	if err := w.Flush(); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		return fail(err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("state compaction rename failed: %w", err)
	}

	if err := s.file.Close(); err != nil {
		return fmt.Errorf("failed to close the old state file after compaction: %w", err)
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("failed to reopen the state file after compaction: %w", err)
	}
	s.file = f
	s.writer = bufio.NewWriter(f)
	s.appended = 0

	// The rename is only durable once the directory itself is synced.
	if dir, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
