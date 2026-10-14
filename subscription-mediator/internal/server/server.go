// Package server exposes the mediator's HTTP surface: the API Portal webhook
// receiver, a liveness probe, and a plain status endpoint.
//
// This is a demo service. The endpoints are unauthenticated and plaintext —
// don't expose the port beyond where the portal and an operator can reach it.
package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"subscription-mediator/internal/config"
	"subscription-mediator/internal/mediator"
	"subscription-mediator/internal/webhook"
)

// ApplyTimeout bounds how long one delivery may spend on gateway calls. It
// stays inside the server's write timeout so the answer still reaches the portal.
const ApplyTimeout = 25 * time.Second

// Server owns the HTTP listener.
type Server struct {
	codec    *webhook.Codec
	mediator *mediator.Mediator
	logger   *slog.Logger
	http     *http.Server
}

// New builds the HTTP server.
func New(codec *webhook.Codec, med *mediator.Mediator, logger *slog.Logger) *Server {
	s := &Server{codec: codec, mediator: med, logger: logger}

	mux := http.NewServeMux()
	mux.HandleFunc("POST "+config.WebhookPath, s.handleWebhook)
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /status", s.handleStatus)

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

// Handler returns the HTTP handler, so tests can drive it without a listener.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// handleWebhook receives one API Portal delivery and applies it before
// answering, as the Platform API does: 200 once the gateway reflects the event,
// an error otherwise. The portal never redelivers, so a non-2xx answer leaves
// the delivery FAILED in its own delivery history, which is where an operator
// finds it.
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

	// Detached from the request: a portal that gives up waiting must not cut an
	// update off halfway. The gateway calls are idempotent, so finishing after
	// the portal has marked the delivery FAILED is harmless.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), ApplyTimeout)
	defer cancel()

	if err := s.mediator.Handle(ctx, env, body); err != nil {
		if mediator.IsPermanent(err) {
			// Retrying the same event cannot succeed; the message says what to fix.
			log.Error("event cannot be applied", slog.Any("error", err))
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error(), "eventId": env.EventID})
			return
		}
		log.Error("event apply failed at the gateway", slog.Any("error", err))
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error(), "eventId": env.EventID})
		return
	}

	log.Info("applied webhook event")
	writeJSON(w, http.StatusOK, map[string]string{"status": "processed", "eventId": env.EventID})
}

// handleHealth is the liveness probe, and backs the container healthcheck.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleStatus reports how many events this process has applied and failed
// since it started.
func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	applied, failed := s.mediator.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"events": map[string]int64{
			"applied": applied,
			"failed":  failed,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
