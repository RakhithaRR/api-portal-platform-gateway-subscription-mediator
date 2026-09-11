// Command subscription-mediator bridges WSO2 API Portal 1.0.0 webhook events to
// the WSO2 API Platform Gateway 1.2.0 Controller management REST API.
//
// It exists for a demo where the gateway is attached to WSO2 API Manager 4.7.0
// rather than the API Portal's Platform API, and therefore never receives the
// portal's subscription and API-key events over the platform-api WebSocket.
//
// Both wire contracts it speaks are version-specific: the API Portal 1.0.0
// webhook envelope and the API Platform Gateway 1.2.0 management REST API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"subscription-mediator/internal/config"
	"subscription-mediator/internal/gateway"
	"subscription-mediator/internal/mediator"
	"subscription-mediator/internal/server"
	"subscription-mediator/internal/store"
	"subscription-mediator/internal/webhook"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "subscription-mediator: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", config.DefaultPath, "path to the TOML configuration file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel(cfg.Logging.Level)}))
	logger.Info("starting subscription-mediator",
		slog.String("config", *configPath),
		slog.String("gateway_base_url", cfg.Gateway.BaseURL),
		slog.Int("api_handle_overrides", len(cfg.APIHandles)))

	// The field-decryption key comes from the same secret that verifies the
	// signature, so a failure here means no token could ever be read.
	codec, err := webhook.NewCodec(cfg.Webhook.Secret)
	if err != nil {
		return err
	}

	st, err := store.Open(config.StatePath, 10000)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := st.Close(); closeErr != nil {
			logger.Error("failed to close the state file", slog.Any("error", closeErr))
		}
	}()

	ready, failed := st.PendingStats()
	logger.Info("loaded state",
		slog.String("path", config.StatePath),
		slog.Int("subscriptions", st.SubscriptionCount()),
		slog.Int("api_keys", st.APIKeyCount()),
		slog.Int("queued_events", ready),
		slog.Int("failed_events", failed))
	if failed > 0 {
		logger.Warn("state holds events that exhausted every attempt; see GET /failed, "+
			"then POST /failed/requeue once the cause is fixed", slog.Int("failed_events", failed))
	}

	client := gateway.New(cfg.Gateway.BaseURL, cfg.Gateway.Username, cfg.Gateway.Password)
	med := mediator.New(client, st, codec, cfg.APIHandles, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A reachability probe gives an immediate, clear diagnostic. It is not a
	// gate: the gateway may come up after the mediator, and an unreachable
	// gateway must not stop the mediator accepting events the portal will never
	// resend.
	probeCtx, cancelProbe := context.WithTimeout(ctx, 15*time.Second)
	if err := client.Ping(probeCtx); err != nil {
		logger.Warn("could not reach the gateway management API; events will be queued and applied "+
			"once it responds", slog.Any("error", err))
	} else {
		logger.Info("the gateway management API is reachable and the credentials were accepted")
	}
	cancelProbe()

	med.Start(ctx)

	srv := server.New(codec, med, st, logger)
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	logger.Info("subscription-mediator is ready",
		slog.Int("workers", mediator.Workers),
		slog.Int("max_pending_events", mediator.MaxPendingEvents))

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server failed: %w", err)
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received; draining")
	}

	// Stop accepting first, then let the workers finish what they hold. An event
	// that does not finish stays on disk and is picked up on the next start.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("the http server did not shut down cleanly", slog.Any("error", err))
	}
	stop()

	drained := make(chan struct{})
	go func() {
		med.Wait()
		close(drained)
	}()
	select {
	case <-drained:
		logger.Info("workers drained")
	case <-time.After(20 * time.Second):
		logger.Warn("workers did not drain in time; unfinished events remain queued")
	}

	applied, permanentFailures := med.Stats()
	logger.Info("subscription-mediator stopped",
		slog.Int64("events_applied", applied),
		slog.Int64("events_failed", permanentFailures))
	return nil
}

// logLevel maps the configured level name onto slog's, defaulting to info.
func logLevel(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
