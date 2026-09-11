// Package store is the mediator's durable state: the portal-to-gateway
// identifier mappings, the processed-event ledger that suppresses duplicate
// deliveries, and the bounded pending queue of events still to be applied.
//
// It is a single append-only JSON-lines log with an in-memory index, compacted
// in place once the log grows past a threshold. That is deliberate: the API
// Portal delivers each event exactly once with no retry, so an event has to
// survive a restart between being accepted and being applied.
//
// Subscription tokens are stored as-is. This is a demo service, so there is no
// encryption at rest — treat the state file as sensitive.
package store

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// ErrQueueFull is returned when the pending queue is at its ceiling.
var ErrQueueFull = errors.New("pending event queue is full")

// record kinds written to the log.
const (
	kindAPIMapping   = "api"
	kindPlanMapping  = "plan"
	kindSubscription = "sub"
	kindAPIKey       = "key"
	kindProcessed    = "processed"
	kindPending      = "pending"
	kindPendingDone  = "pending_done"
)

// PendingState is the lifecycle of a queued event.
type PendingState string

// Pending event states.
const (
	// StateReady means the event is waiting to be applied, possibly after a backoff.
	StateReady PendingState = "READY"
	// StateFailed means every attempt was exhausted. It stays in the file for an
	// operator to inspect and requeue, and no longer occupies queue capacity.
	StateFailed PendingState = "FAILED"
)

// Subscription is the mediator's record of one portal subscription and the
// gateway subscription it became.
type Subscription struct {
	PortalSubscriptionID  string    `json:"portalSubscriptionId"`
	GatewaySubscriptionID string    `json:"gatewaySubscriptionId"`
	GatewayAPIHandle      string    `json:"gatewayApiHandle"`
	GatewayPlanID         string    `json:"gatewayPlanId,omitempty"`
	PortalPlanRefID       string    `json:"portalPlanRefId,omitempty"`
	PortalPlanName        string    `json:"portalPlanName,omitempty"`
	ApplicationID         string    `json:"applicationId,omitempty"`
	SubscriberID          string    `json:"subscriberId,omitempty"`
	Status                string    `json:"status"`
	APIName               string    `json:"apiName,omitempty"`
	APIVersion            string    `json:"apiVersion,omitempty"`
	UpdatedAt             time.Time `json:"updatedAt"`
	// Token is kept because the gateway offers no way to change a subscription's
	// plan or replace its token: both have to be a delete followed by a create
	// with a token, so it must still be available.
	Token string `json:"token,omitempty"`
}

// APIKey is the mediator's record of one portal API key and its gateway name.
type APIKey struct {
	PortalKeyID      string    `json:"portalKeyId"`
	GatewayAPIHandle string    `json:"gatewayApiHandle"`
	GatewayKeyName   string    `json:"gatewayKeyName"`
	PortalHandle     string    `json:"portalHandle,omitempty"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

// Pending is a durably queued event awaiting application to the gateway.
type Pending struct {
	Seq         uint64       `json:"seq"`
	EventID     string       `json:"eventId"`
	EventType   string       `json:"eventType"`
	OrderingKey string       `json:"orderingKey"`
	Body        []byte       `json:"body"`
	ReceivedAt  time.Time    `json:"receivedAt"`
	Attempts    int          `json:"attempts"`
	NextAttempt time.Time    `json:"nextAttempt"`
	State       PendingState `json:"state"`
	LastError   string       `json:"lastError,omitempty"`
}

// entry is one line in the log.
type entry struct {
	Kind      string          `json:"kind"`
	Key       string          `json:"key"`
	Deleted   bool            `json:"deleted,omitempty"`
	Timestamp time.Time       `json:"ts"`
	Value     json.RawMessage `json:"value,omitempty"`
}

// Store is the durable state. All methods are safe for concurrent use.
type Store struct {
	mu               sync.RWMutex
	path             string
	file             *os.File
	writer           *bufio.Writer
	appended         int
	compactThreshold int
	seq              uint64

	apiMappings  map[string]string
	planMappings map[string]string
	subs         map[string]*Subscription
	keys         map[string]*APIKey
	processed    map[string]time.Time
	pending      map[uint64]*Pending
}

// Open loads the state file, replaying it into memory, and returns a Store
// ready for appends. A missing file is created.
func Open(path string, compactThreshold int) (*Store, error) {
	if compactThreshold <= 0 {
		compactThreshold = 10000
	}
	s := &Store{
		path:             path,
		compactThreshold: compactThreshold,
		apiMappings:      map[string]string{},
		planMappings:     map[string]string{},
		subs:             map[string]*Subscription{},
		keys:             map[string]*APIKey{},
		processed:        map[string]time.Time{},
		pending:          map[uint64]*Pending{},
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create the state directory: %w", err)
		}
	}
	if err := s.replay(); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("failed to open the state file: %w", err)
	}
	s.file = f
	s.writer = bufio.NewWriter(f)
	return s, nil
}

// replay rebuilds the in-memory index from the log.
//
// A truncated final line (a crash mid-append) is tolerated: parsing stops there
// and everything before it is kept, which is the point of an append-only log.
func (s *Store) replay() error {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to open the state file for replay: %w", err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		raw := scanner.Bytes()
		if len(raw) == 0 {
			continue
		}
		var e entry
		if err := json.Unmarshal(raw, &e); err != nil {
			// A partial trailing line is expected after an unclean shutdown;
			// anything earlier is real corruption.
			if scanner.Scan() {
				return fmt.Errorf("state file %s is corrupt at line %d", s.path, line)
			}
			break
		}
		if err := s.applyEntry(&e); err != nil {
			return fmt.Errorf("state file %s line %d: %w", s.path, line, err)
		}
		s.appended++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("failed to read the state file: %w", err)
	}
	return nil
}

// applyEntry folds one log entry into the in-memory index.
func (s *Store) applyEntry(e *entry) error {
	switch e.Kind {
	case kindAPIMapping:
		if e.Deleted {
			delete(s.apiMappings, e.Key)
			return nil
		}
		var handle string
		if err := json.Unmarshal(e.Value, &handle); err != nil {
			return err
		}
		s.apiMappings[e.Key] = handle
	case kindPlanMapping:
		if e.Deleted {
			delete(s.planMappings, e.Key)
			return nil
		}
		var planID string
		if err := json.Unmarshal(e.Value, &planID); err != nil {
			return err
		}
		s.planMappings[e.Key] = planID
	case kindSubscription:
		if e.Deleted {
			delete(s.subs, e.Key)
			return nil
		}
		var sub Subscription
		if err := json.Unmarshal(e.Value, &sub); err != nil {
			return err
		}
		s.subs[e.Key] = &sub
	case kindAPIKey:
		if e.Deleted {
			delete(s.keys, e.Key)
			return nil
		}
		var k APIKey
		if err := json.Unmarshal(e.Value, &k); err != nil {
			return err
		}
		s.keys[e.Key] = &k
	case kindProcessed:
		s.processed[e.Key] = e.Timestamp
	case kindPending:
		var p Pending
		if err := json.Unmarshal(e.Value, &p); err != nil {
			return err
		}
		s.pending[p.Seq] = &p
		if p.Seq > s.seq {
			s.seq = p.Seq
		}
	case kindPendingDone:
		var seq uint64
		if err := json.Unmarshal(e.Value, &seq); err != nil {
			return err
		}
		delete(s.pending, seq)
		if seq > s.seq {
			s.seq = seq
		}
	default:
		// An unknown kind comes from a newer build writing the same file. Ignore
		// it rather than refusing to start.
	}
	return nil
}

// appendLocked writes one entry and flushes it to disk. Callers hold the lock.
func (s *Store) appendLocked(e *entry) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("failed to encode the state entry: %w", err)
	}
	if _, err := s.writer.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("failed to write the state entry: %w", err)
	}
	if err := s.writer.Flush(); err != nil {
		return fmt.Errorf("failed to flush the state entry: %w", err)
	}
	// fsync so an accepted-but-unapplied event survives a hard restart. The
	// portal never redelivers, so losing this write loses the event.
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("failed to sync the state file: %w", err)
	}
	s.appended++
	if s.appended >= s.compactThreshold {
		return s.compactLocked()
	}
	return nil
}

func marshalValue(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("failed to encode the state value: %w", err)
	}
	return raw, nil
}

// Close flushes and closes the state file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer != nil {
		if err := s.writer.Flush(); err != nil {
			return err
		}
	}
	if s.file != nil {
		if err := s.file.Sync(); err != nil {
			return err
		}
		return s.file.Close()
	}
	return nil
}
