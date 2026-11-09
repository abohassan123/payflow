// Package metrics exposes Prometheus metrics. Prometheus (Phase 12) scrapes
// GET /metrics every few seconds; Grafana draws dashboards from it.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	PaymentsByStatus = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payflow_sim_payment_transitions_total",
		Help: "Payments that reached each status.",
	}, []string{"status"})

	Refunds = promauto.NewCounter(prometheus.CounterOpts{
		Name: "payflow_sim_refunds_total",
		Help: "Refunds created.",
	})

	DuplicateRequests = promauto.NewCounter(prometheus.CounterOpts{
		Name: "payflow_sim_duplicate_requests_total",
		Help: "Requests re-sent with an existing idempotency key (should create nothing).",
	})

	FraudBursts = promauto.NewCounter(prometheus.CounterOpts{
		Name: "payflow_sim_fraud_bursts_total",
		Help: "Fraud bursts injected.",
	})

	Dropped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "payflow_sim_dropped_ticks_total",
		Help: "Ticks skipped because MaxInFlight was reached (back-pressure).",
	})

	InFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "payflow_sim_in_flight",
		Help: "Payment lifecycles currently running.",
	})

	DBErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payflow_sim_db_errors_total",
		Help: "Database errors by operation (after retries).",
	}, []string{"op"})

	DBLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "payflow_sim_db_op_seconds",
		Help:    "Database operation latency.",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 12), // 1ms .. ~2s
	}, []string{"op"})
)
