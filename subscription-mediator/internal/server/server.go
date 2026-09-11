// Package server exposes the mediator's HTTP surface: the API Portal webhook
// receiver, a liveness probe, and a few plain diagnostic endpoints.
//
// This is a demo service. The endpoints are unauthenticated and plaintext —
// don't expose the port beyond where the portal and an operator can reach it.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"subscription-mediator/internal/config"
	"subscription-mediator/internal/mediator"
	"subscription-mediator/internal/store"
	"subscription-mediator/internal/webhook"
)

// Server owns the HTTP listener.
type Server struct {
	codec    *webhook.Codec
	mediator *mediator.Mediator
	store    *store.Store
	logger   *slog.Logger
	http     *http.Server
}

// New builds the HTTP server.
func New(codec *webhook.Codec, med *mediator.Mediator, st *store.Store, logger *slog.Logger) *Server {
	s := &Server{codec: codec, mediator: med, store: st, logger: logger}

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+config.WebhookPath, s.handleWebhook)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /failed", s.handleFailed)
	mux.HandleFunc("POST /failed/requeue", s.handleRequeue)

	s.http = &http.Server{
		Addr:    config.ListenAddress,
		Handler: mux,
		// Modest timeouts so a stalled connection cannot hold a goroutine for
		// the life of the process.
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	return s
}

// ListenAndServe serves until the listener is closed.
func (s *Server) ListenAndServe() error {
	s.logger.Info("listening for API Portal webhooks",
		slog.String("address", config.ListenAddress),
		slog.String("path", config.WebhookPath))
	return s.http.ListenAndServe()
}

// Shutdown stops accepting new requests and waits out the in-flight ones.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

// handleWebhook receives one API Portal delivery.
//
// The delivery is verified, decoded, and durably queued before it is
// acknowledged. The portal attempts each delivery exactly once and never
// redelivers, so acknowledging before the event is on disk would lose it on a
// restart.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, config.MaxBodyBytes))
	if err != nil {
		s.logger.Warn("failed to read a webhook delivery", slog.Any("error", err))
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the request body could not be read"})
		return
	}

	// The signature covers the raw bytes, so it is checked before anything
	// parses them.
	if err := s.codec.Verify(r.Header.Get("X-Api-Portal-Signature"), body, time.Now()); err != nil {
		s.logger.Warn("rejected an unverified webhook delivery",
			slog.String("remote_addr", r.RemoteAddr))
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid webhook signature"})
		return
	}

	env, err := webhook.Decode(body)
	if err != nil {
		s.logger.Warn("rejected a malformed webhook envelope", slog.Any("error", err))
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	log := s.logger.With(
		slog.String("event_id", env.EventID),
		slog.String("event_type", env.EventType),
	)

	queued, err := s.mediator.Accept(env, body)
	if err != nil {
		if errors.Is(err, store.ErrQueueFull) {
			// The bounded queue is full. Rejecting is the honest answer: the
			// portal marks the delivery failed, which is visible in its own
			// webhook-event listing, rather than the queue growing without limit.
			log.Error("rejected a delivery because the pending queue is full")
			w.Header().Set("Retry-After", "30")
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the event queue is full"})
			return
		}
		log.Error("failed to queue a webhook delivery", slog.Any("error", err))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the event could not be accepted"})
		return
	}

	if !queued {
		log.Info("duplicate of an already-applied event; acknowledging")
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate", "eventId": env.EventID})
		return
	}

	log.Info("queued webhook delivery")
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "eventId": env.EventID})
}

// handleHealth is the liveness probe, and backs the container healthcheck.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleStatus reports queue depth and mapping counts.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	ready, failed := s.store.PendingStats()
	applied, permanentFailures := s.mediator.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"queue": map[string]any{
			"ready":       ready,
			"failed":      failed,
			"applied":     applied,
			"failedFinal": permanentFailures,
		},
		"mappings": map[string]int{
			"subscriptions": s.store.SubscriptionCount(),
			"apiKeys":       s.store.APIKeyCount(),
			"processed":     s.store.ProcessedCount(),
		},
	})
}

// handleFailed lists events that exhausted every attempt, so it is obvious what
// needs attention and why.
func (s *Server) handleFailed(w http.ResponseWriter, _ *http.Request) {
	type view struct {
		Seq        uint64    `json:"seq"`
		EventID    string    `json:"eventId"`
		EventType  string    `json:"eventType"`
		Attempts   int       `json:"attempts"`
		ReceivedAt time.Time `json:"receivedAt"`
		LastError  string    `json:"lastError"`
	}
	records := s.store.FailedPending(200)
	out := make([]view, 0, len(records))
	for _, record := range records {
		out = append(out, view{
			Seq: record.Seq, EventID: record.EventID, EventType: record.EventType,
			Attempts: record.Attempts, ReceivedAt: record.ReceivedAt, LastError: record.LastError,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "failed": out})
}

// handleRequeue returns every failed event to the queue, for after whatever
// blocked them has been fixed.
func (s *Server) handleRequeue(w http.ResponseWriter, _ *http.Request) {
	requeued, err := s.store.RequeueFailed()
	if err != nil {
		s.logger.Error("failed to requeue failed events", slog.Any("error", err))
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "the events could not be requeued"})
		return
	}
	// Records that just became due carry no new arrival to wake the dispatcher.
	s.mediator.Nudge()
	s.logger.Info("requeued failed events", slog.Int("count", requeued))
	writeJSON(w, http.StatusOK, map[string]any{"requeued": requeued})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
