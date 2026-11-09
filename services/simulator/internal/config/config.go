// Package config loads settings from environment variables (12-factor app style):
// the same Docker image runs locally, in CI and in Kubernetes, only env differs.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL     string  // postgres://user:pw@host:5432/db?sslmode=disable
	TPS             float64 // new payments per second
	MaxInFlight     int     // max concurrent payment lifecycles (back-pressure)
	Merchants       int     // merchants to seed on first start
	Customers       int     // customers to seed on first start
	FraudRate       float64 // probability that a tick starts a fraud burst instead of a normal payment
	RefundRate      float64 // share of captured payments that get refunded
	DuplicateRate   float64 // share of requests re-sent with the same idempotency key
	FeeBps          int64   // processing fee in basis points
	Seed            uint64  // 0 = random; any other value = reproducible run
	MetricsAddr     string  // where /metrics and /healthz listen
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
}

// Load reads the environment, applies defaults and validates.
func Load() (Config, error) {
	var errs []error
	c := Config{
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		TPS:             floatEnv("SIM_TPS", 5, &errs),
		MaxInFlight:     intEnv("SIM_MAX_IN_FLIGHT", 500, &errs),
		Merchants:       intEnv("SIM_MERCHANTS", 50, &errs),
		Customers:       intEnv("SIM_CUSTOMERS", 2000, &errs),
		FraudRate:       floatEnv("SIM_FRAUD_RATE", 0.01, &errs),
		RefundRate:      floatEnv("SIM_REFUND_RATE", 0.05, &errs),
		DuplicateRate:   floatEnv("SIM_DUPLICATE_RATE", 0.02, &errs),
		FeeBps:          int64(intEnv("SIM_FEE_BPS", 250, &errs)),
		Seed:            uint64(intEnv("SIM_SEED", 0, &errs)),
		MetricsAddr:     envOr("METRICS_ADDR", ":2112"),
		ShutdownTimeout: time.Duration(intEnv("SHUTDOWN_TIMEOUT_SECONDS", 15, &errs)) * time.Second,
	}

	switch strings.ToLower(envOr("LOG_LEVEL", "info")) {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "info":
		c.LogLevel = slog.LevelInfo
	case "warn":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		errs = append(errs, fmt.Errorf("LOG_LEVEL must be debug|info|warn|error"))
	}

	if c.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if c.TPS <= 0 || c.TPS > 1000 {
		errs = append(errs, fmt.Errorf("SIM_TPS must be in (0, 1000], got %v", c.TPS))
	}
	for name, v := range map[string]float64{
		"SIM_FRAUD_RATE": c.FraudRate, "SIM_REFUND_RATE": c.RefundRate, "SIM_DUPLICATE_RATE": c.DuplicateRate,
	} {
		if v < 0 || v > 1 {
			errs = append(errs, fmt.Errorf("%s must be between 0 and 1, got %v", name, v))
		}
	}
	if c.Merchants < 1 || c.Customers < 1 || c.MaxInFlight < 1 {
		errs = append(errs, errors.New("SIM_MERCHANTS, SIM_CUSTOMERS and SIM_MAX_IN_FLIGHT must be >= 1"))
	}
	return c, errors.Join(errs...)
}

// Redacted returns the config safe to log (no password).
func (c Config) Redacted() Config {
	if i := strings.Index(c.DatabaseURL, "@"); i > 0 {
		if j := strings.LastIndex(c.DatabaseURL[:i], ":"); j > strings.Index(c.DatabaseURL, "//") {
			c.DatabaseURL = c.DatabaseURL[:j+1] + "****" + c.DatabaseURL[i:]
		}
	}
	return c
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func intEnv(key string, def int, errs *[]error) int {
	v := envOr(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %q is not an integer", key, v))
		return def
	}
	return n
}

func floatEnv(key string, def float64, errs *[]error) float64 {
	v := envOr(key, "")
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: %q is not a number", key, v))
		return def
	}
	return f
}
