// Command simulator generates realistic payment traffic into PostgreSQL.
//
// Startup order: config -> logger -> DB (wait until ready) -> seed reference
// data once -> HTTP server for /metrics and /healthz -> run until SIGINT/SIGTERM.
package main

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/payflow/simulator/internal/config"
	"github.com/payflow/simulator/internal/sim"
	"github.com/payflow/simulator/internal/store"
)

func main() {
	if err := run(); err != nil {
		slog.Error("simulator exited with error", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Structured JSON logs: one event per line, machine-parseable (Loki, CloudWatch...).
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})).
		With("service", "payflow-simulator")
	slog.SetDefault(log)
	log.Info("starting", "config", cfg.Redacted())

	// ctx is cancelled on Ctrl+C (SIGINT) or `docker stop` (SIGTERM).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	seed := cfg.Seed
	if seed == 0 {
		seed = rand.Uint64()
	}
	log.Info("random seed", "seed", seed) // log it so any run can be reproduced
	master := rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))

	st, err := store.New(ctx, cfg.DatabaseURL, 60*time.Second, log)
	if err != nil {
		return err
	}
	defer st.Close()

	merchants, customers, instruments := sim.GenerateActors(master, cfg.Merchants, cfg.Customers)
	seeded, err := st.SeedIfEmpty(ctx, merchants, customers, instruments)
	if err != nil {
		return err
	}
	log.Info("reference data ready", "seeded_now", seeded)

	// Always load from the DB: after a restart we reuse existing actors.
	if merchants, err = st.LoadMerchants(ctx); err != nil {
		return err
	}
	if instruments, err = st.LoadInstruments(ctx); err != nil {
		return err
	}
	log.Info("actors loaded", "merchants", len(merchants), "instruments", len(instruments))

	srv := newHTTPServer(cfg.MetricsAddr, st)
	go func() {
		log.Info("http listening", "addr", cfg.MetricsAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "err", err)
			stop()
		}
	}()

	runErr := sim.New(cfg, st, log, master, merchants, instruments).Run(ctx)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return runErr
}

// newHTTPServer exposes:
//
//	/metrics  Prometheus scrape endpoint
//	/healthz  liveness+readiness: 200 if the DB answers, 503 otherwise
//	          (Kubernetes probes will call this in Phase 9)
func newHTTPServer(addr string, st *store.Store) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := st.Ping(ctx); err != nil {
			http.Error(w, "db unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	return &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
}
