// Command wispd is the Wisp broker daemon entrypoint.
//
// Wisp leases ephemeral, root-access, throwaway containers for a bounded time
// (see docs/DESIGN.md). This process wires together structured logging,
// environment-driven config, the policy engine, the Docker-backed runtime, the
// contract store, the event bus, the TTL reaper, and the HTTP broker surface,
// then serves it until a signal triggers a graceful shutdown.
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
	"github.com/benjaminfkile/wisp/internal/policy"
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

	pol, err := policy.Load(cfg.ImageConfigFile)
	if err != nil {
		logger.Error("load policy config", "error", err)
		os.Exit(1)
	}

	rt, err := runtime.NewDockerRuntime()
	if err != nil {
		logger.Error("init docker runtime", "error", err)
		os.Exit(1)
	}
	defer func() { _ = rt.Close() }()

	if err := run(logger, cfg, rt, pol); err != nil {
		logger.Error("server exited", "error", err)
		os.Exit(1)
	}
}

// run starts the HTTP server and blocks until an interrupt triggers a graceful
// shutdown.
func run(logger *slog.Logger, cfg config.Config, rt runtime.Runtime, pol *policy.Config) error {
	store := contract.NewStore()

	// The event bus is shared: the HTTP surface publishes contract.created /
	// .ready / .released and serves /events, while the reaper publishes
	// contract.expiring / .expired through the same bus (see docs/DESIGN.md §6).
	eventBus := bus.New(logger)

	// The daemon bundles the HTTP handler with the shared store/runtime/allocator
	// so the reconcile and the reaper below operate on the SAME GPU device
	// allocator the create path allocates from (see server.NewDaemon).
	daemon := server.NewDaemon(logger, store, rt, pol, eventBus, cfg.AppToken)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           daemon.Handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Rebuild tracking for containers a previous wispd left behind (matched by
	// their wisp.contract label), BEFORE the reaper starts, so an orphaned lease
	// is either reaped or resumed on the reaper's first sweep instead of running
	// unbounded with no daemon enforcing its TTL. This also rebuilds the GPU
	// allocator's occupancy from the wisp.gpus labels so a restart never
	// double-assigns a device a surviving lease still holds, and re-Reserves each
	// surviving lease's cpus/memory from the wisp.cpus and wisp.memory_mb labels so
	// a restart never oversubscribes the host by under-counting live leases (see
	// daemon.Reconcile).
	daemon.Reconcile(ctx)

	// The TTL reaper reconciles tracked contracts on boot and then drives
	// expiring/expired transitions on a ticker until shutdown. Its lifecycle
	// hook republishes those transitions onto the event bus; its ReleaseGPUs hook
	// returns an expired lease's devices to the GPU allocator, and its
	// ReleaseCapacity hook returns its reserved cpus / memory to the aggregate
	// capacity allocator.
	rp := reaper.New(store, rt, reaper.Options{
		Lead:            cfg.ExpiringLead,
		Interval:        cfg.ReapInterval,
		Logger:          logger,
		Notify:          server.LifecycleNotify(eventBus, logger),
		ReleaseGPUs:     daemon.ReleaseGPUs,
		ReleaseCapacity: daemon.ReleaseCapacity,
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
