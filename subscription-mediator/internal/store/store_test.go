package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T, dir string, threshold int) *Store {
	t.Helper()
	st, err := Open(filepath.Join(dir, "state.jsonl"), threshold)
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	return st
}

// TestStateSurvivesRestart is the property the whole design rests on: the API
// Portal never redelivers, so an accepted event and its mappings must still be
// there after a restart.
func TestStateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()

	st := openTest(t, dir, 10000)
	if err := st.PutSubscription(&Subscription{
		PortalSubscriptionID:  "portal-sub-1",
		GatewaySubscriptionID: "gw-sub-1",
		GatewayAPIHandle:      "order-api-v1.0",
		Status:                "ACTIVE",
		Token:                 "sub-token-value-1234567890",
	}); err != nil {
		t.Fatalf("PutSubscription() = %v", err)
	}
	if err := st.PutAPIMapping("cp-api-1", "order-api-v1.0"); err != nil {
		t.Fatalf("PutAPIMapping() = %v", err)
	}
	if _, err := st.Enqueue(&Pending{EventID: "e1", EventType: "subscription.created", Body: []byte(`{"a":1}`)}, 100); err != nil {
		t.Fatalf("Enqueue() = %v", err)
	}
	if err := st.MarkProcessed("e0"); err != nil {
		t.Fatalf("MarkProcessed() = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	reopened := openTest(t, dir, 10000)
	defer reopened.Close()

	sub, ok := reopened.Subscription("portal-sub-1")
	if !ok {
		t.Fatal("the subscription did not survive a restart")
	}
	if sub.GatewaySubscriptionID != "gw-sub-1" {
		t.Errorf("GatewaySubscriptionID = %q", sub.GatewaySubscriptionID)
	}
	// The token has to survive: a later plan change recreates the subscription
	// with it, and there is no other copy.
	if sub.Token != "sub-token-value-1234567890" {
		t.Errorf("the token did not survive a restart: %q", sub.Token)
	}
	if handle, ok := reopened.APIMapping("cp-api-1"); !ok || handle != "order-api-v1.0" {
		t.Errorf("APIMapping() = %q, %v", handle, ok)
	}
	if !reopened.AlreadyProcessed("e0") {
		t.Error("the processed-event ledger did not survive a restart")
	}
	if ready, _ := reopened.PendingStats(); ready != 1 {
		t.Errorf("queued events after restart = %d, want 1", ready)
	}
}

// TestEnqueueRejectsBeyondTheCeiling asserts the pending queue is bounded, so a
// gateway outage cannot grow it without limit.
func TestEnqueueRejectsBeyondTheCeiling(t *testing.T) {
	st := openTest(t, t.TempDir(), 10000)
	defer st.Close()

	for i := 0; i < 3; i++ {
		if _, err := st.Enqueue(&Pending{EventID: fmt.Sprintf("e%d", i)}, 3); err != nil {
			t.Fatalf("Enqueue() %d = %v", i, err)
		}
	}
	if _, err := st.Enqueue(&Pending{EventID: "overflow"}, 3); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("Enqueue() past the ceiling = %v, want ErrQueueFull", err)
	}
	// A completed record frees capacity again.
	if err := st.CompletePending(1); err != nil {
		t.Fatalf("CompletePending() = %v", err)
	}
	if _, err := st.Enqueue(&Pending{EventID: "after-drain"}, 3); err != nil {
		t.Fatalf("Enqueue() after draining = %v, want nil", err)
	}
}

// TestFailedRecordsDoNotOccupyQueueCapacity asserts a permanently failed event
// does not starve new ones.
func TestFailedRecordsDoNotOccupyQueueCapacity(t *testing.T) {
	st := openTest(t, t.TempDir(), 10000)
	defer st.Close()

	record, err := st.Enqueue(&Pending{EventID: "e1"}, 1)
	if err != nil {
		t.Fatalf("Enqueue() = %v", err)
	}
	record.State = StateFailed
	record.LastError = "no matching gateway API"
	if err := st.UpdatePending(record); err != nil {
		t.Fatalf("UpdatePending() = %v", err)
	}
	if _, err := st.Enqueue(&Pending{EventID: "e2"}, 1); err != nil {
		t.Fatalf("Enqueue() alongside a failed record = %v, want nil", err)
	}
	if ready, failed := st.PendingStats(); ready != 1 || failed != 1 {
		t.Fatalf("PendingStats() = %d ready, %d failed; want 1, 1", ready, failed)
	}
}

func TestRequeueFailedResetsAttempts(t *testing.T) {
	st := openTest(t, t.TempDir(), 10000)
	defer st.Close()

	record, _ := st.Enqueue(&Pending{EventID: "e1"}, 10)
	record.State = StateFailed
	record.Attempts = 6
	record.LastError = "gateway unreachable"
	if err := st.UpdatePending(record); err != nil {
		t.Fatalf("UpdatePending() = %v", err)
	}

	requeued, err := st.RequeueFailed()
	if err != nil || requeued != 1 {
		t.Fatalf("RequeueFailed() = %d, %v", requeued, err)
	}
	got, ok := st.Pending(record.Seq)
	if !ok || got.State != StateReady || got.Attempts != 0 || got.LastError != "" {
		t.Fatalf("the requeued record was not reset: %+v", got)
	}
}

// TestDuePendingRespectsBackoffAndOrder asserts records come back in delivery
// order and only once their backoff has elapsed — the two properties the worker
// lanes rely on for ordering.
func TestDuePendingRespectsBackoffAndOrder(t *testing.T) {
	st := openTest(t, t.TempDir(), 10000)
	defer st.Close()

	first, _ := st.Enqueue(&Pending{EventID: "e1", OrderingKey: "sub:1"}, 10)
	second, _ := st.Enqueue(&Pending{EventID: "e2", OrderingKey: "sub:1"}, 10)

	first.NextAttempt = time.Now().Add(time.Hour)
	if err := st.UpdatePending(first); err != nil {
		t.Fatalf("UpdatePending() = %v", err)
	}

	due := st.DuePending(time.Now(), 0)
	if len(due) != 1 || due[0].Seq != second.Seq {
		t.Fatalf("DuePending() returned %d records, want only the one without a backoff", len(due))
	}
	due = st.DuePending(time.Now().Add(2*time.Hour), 0)
	if len(due) != 2 || due[0].Seq != first.Seq || due[1].Seq != second.Seq {
		t.Fatal("DuePending() did not return both in sequence order once due")
	}
}

// TestCompactionPreservesStateAndSequence asserts a compaction does not lose
// live records or reissue sequence numbers.
func TestCompactionPreservesStateAndSequence(t *testing.T) {
	dir := t.TempDir()
	st := openTest(t, dir, 5) // compact aggressively so the test exercises it

	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("api-%d", i%5)
		if err := st.PutAPIMapping(key, "handle-"+key); err != nil {
			t.Fatalf("PutAPIMapping() = %v", err)
		}
	}
	if err := st.PutSubscription(&Subscription{
		PortalSubscriptionID: "s1", GatewaySubscriptionID: "g1", Token: "tok",
	}); err != nil {
		t.Fatalf("PutSubscription() = %v", err)
	}
	last, err := st.Enqueue(&Pending{EventID: "e-final"}, 100)
	if err != nil {
		t.Fatalf("Enqueue() = %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	reopened := openTest(t, dir, 10000)
	defer reopened.Close()

	if handle, ok := reopened.APIMapping("api-0"); !ok || handle != "handle-api-0" {
		t.Errorf("APIMapping() after compaction = %q, %v", handle, ok)
	}
	if sub, ok := reopened.Subscription("s1"); !ok || sub.Token != "tok" {
		t.Errorf("the subscription did not survive compaction: %+v", sub)
	}
	next, err := reopened.Enqueue(&Pending{EventID: "e-after"}, 100)
	if err != nil {
		t.Fatalf("Enqueue() after compaction = %v", err)
	}
	if next.Seq <= last.Seq {
		t.Fatalf("the sequence went backwards across compaction: %d then %d", last.Seq, next.Seq)
	}
}

func TestGatewayKeyNameTaken(t *testing.T) {
	st := openTest(t, t.TempDir(), 10000)
	defer st.Close()

	if err := st.PutAPIKey(&APIKey{PortalKeyID: "k1", GatewayAPIHandle: "api-a", GatewayKeyName: "my-key"}); err != nil {
		t.Fatalf("PutAPIKey() = %v", err)
	}
	if st.GatewayKeyNameTaken("api-a", "my-key", "k1") {
		t.Error("reported a collision with the key itself")
	}
	if !st.GatewayKeyNameTaken("api-a", "my-key", "k2") {
		t.Error("missed a genuine collision")
	}
	if st.GatewayKeyNameTaken("api-b", "my-key", "k2") {
		t.Error("reported a collision across different APIs")
	}
}
