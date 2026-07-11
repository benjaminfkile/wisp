// Command wispd is the Wisp broker daemon entrypoint.
//
// Wisp leases ephemeral, root-access, throwaway containers for a bounded time
// (see docs/DESIGN.md). This scaffold stands up the HTTP server, structured
// logging, and environment-driven config; the broker surface is filled in by
// later tasks.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/benjaminfkile/wisp/internal/bus"
	"github.com/benjaminfkile/wisp/internal/config"
	"github.com/benjaminfkile/wisp/internal/contract"
	"github.com/benjaminfkile/wisp/internal/preset"
	"github.com/benjaminfkile/wisp/internal/reaper"
	"github.com/benjaminfkile/wisp/internal/runtime"
	"github.com/benjaminfkile/wisp/internal/server"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}

	presets, err := preset.Load(cfg.PresetsFile)
	if err != nil {
		logger.Error("load presets", "error", err)
		os.Exit(1)
	}

	rt, err := runtime.NewDockerRuntime()
	if err != nil {
		logger.Error("init docker runtime", "error", err)
		os.Exit(1)
	}
	defer func() { _ = rt.Close() }()

	if err := run(logger, cfg, rt, presets); err != nil {
		logger.Error("server exited", "error", err)
		os.Exit(1)
	}
}

// run starts the HTTP server and blocks until an interrupt triggers a graceful
// shutdown.
func run(logger *slog.Logger, cfg config.Config, rt runtime.Runtime, presets *preset.Set) error {
	store := contract.NewStore()

	// The event bus is shared: the HTTP surface publishes contract.created /
	// .ready / .released and serves /events, while the reaper publishes
	// contract.expiring / .expired through the same bus (see docs/DESIGN.md §6).
	eventBus := bus.New(logger)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.New(logger, store, rt, presets, eventBus, cfg.AppToken),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The TTL reaper reconciles tracked contracts on boot and then drives
	// expiring/expired transitions on a ticker until shutdown. Its lifecycle
	// hook republishes those transitions onto the event bus.
	rp := reaper.New(store, rt, reaper.Options{
		Lead:     cfg.ExpiringLead,
		Interval: cfg.ReapInterval,
		Logger:   logger,
		Notify:   server.LifecycleNotify(eventBus, logger),
	})
	go rp.Run(ctx)

	errc := make(chan error, 1)
	go func() {
		logger.Info("wisp listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
