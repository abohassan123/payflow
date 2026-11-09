// Package sim drives realistic payment traffic against the store.
//
// Concurrency model:
//   - ONE ticker goroutine decides what happens next (new payment, duplicate
//     request, fraud burst). Only it touches the master RNG and the recent list,
//     so no locks are needed.
//   - Each payment lifecycle runs in its own goroutine with its own child RNG.
//   - A buffered channel (semaphore) caps concurrent lifecycles: if the DB slows
//     down we skip ticks instead of piling up unbounded goroutines (back-pressure).
package sim

import (
	"context"
	"encoding/hex"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/payflow/simulator/internal/config"
	"github.com/payflow/simulator/internal/metrics"
	"github.com/payflow/simulator/internal/money"
	"github.com/payflow/simulator/internal/payment"
	"github.com/payflow/simulator/internal/store"
)

type Simulator struct {
	cfg         config.Config
	st          *store.Store
	log         *slog.Logger
	master      *rand.Rand
	merchants   []store.Merchant
	instruments []store.Instrument

	sem    chan struct{}
	wg     sync.WaitGroup
	recent []store.Payment // last N requests, for duplicate re-sends
}

func New(cfg config.Config, st *store.Store, log *slog.Logger, master *rand.Rand,
	merchants []store.Merchant, instruments []store.Instrument) *Simulator {
	return &Simulator{
		cfg: cfg, st: st, log: log, master: master,
		merchants: merchants, instruments: instruments,
		sem: make(chan struct{}, cfg.MaxInFlight),
	}
}

// attempt is one payment request plus what the "card network" will decide.
type attempt struct {
	store.Payment
	approvalRate float64
	fraud        bool
}

// Run ticks until ctx is cancelled, then gives in-flight payments up to
// ShutdownTimeout to finish before cancelling them too (graceful shutdown).
func (s *Simulator) Run(ctx context.Context) error {
	if len(s.merchants) == 0 || len(s.instruments) == 0 {
		return errors.New("no merchants or instruments loaded")
	}
	workCtx, cancelWork := context.WithCancel(context.Background())
	defer cancelWork()

	interval := time.Duration(float64(time.Second) / s.cfg.TPS)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.log.Info("simulator started", "tps", s.cfg.TPS, "tick", interval.String())

	for {
		select {
		case <-ctx.Done():
			s.log.Info("stopping: waiting for in-flight payments", "timeout", s.cfg.ShutdownTimeout.String())
			done := make(chan struct{})
			go func() { s.wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(s.cfg.ShutdownTimeout):
				s.log.Warn("shutdown timeout: cancelling in-flight payments")
				cancelWork()
				<-done
			}
			s.log.Info("simulator stopped")
			return nil
		case <-ticker.C:
			s.tick(workCtx)
		}
	}
}

func (s *Simulator) tick(ctx context.Context) {
	r := s.master
	switch {
	case r.Float64() < s.cfg.FraudRate:
		s.spawn(ctx, s.fraudBurst)
	case len(s.recent) > 20 && r.Float64() < s.cfg.DuplicateRate:
		// Skip the newest 20: their original insert may still be in flight,
		// and we want to re-send requests the server has definitely seen.
		dup := s.recent[r.IntN(len(s.recent)-20)]
		s.spawn(ctx, func(ctx context.Context, _ *rand.Rand) { s.duplicate(ctx, dup) })
	default:
		a := s.newAttempt(r)
		s.remember(a.Payment)
		s.spawn(ctx, func(ctx context.Context, rr *rand.Rand) { s.lifecycle(ctx, rr, a) })
	}
}

// spawn starts fn in a goroutine if there is capacity, with its own RNG.
func (s *Simulator) spawn(ctx context.Context, fn func(context.Context, *rand.Rand)) {
	select {
	case s.sem <- struct{}{}:
	default:
		metrics.Dropped.Inc()
		return
	}
	child := rand.New(rand.NewPCG(s.master.Uint64(), s.master.Uint64()))
	s.wg.Add(1)
	metrics.InFlight.Inc()
	go func() {
		defer func() {
			<-s.sem
			metrics.InFlight.Dec()
			s.wg.Done()
		}()
		fn(ctx, child)
	}()
}

func (s *Simulator) remember(p store.Payment) {
	const keep = 200
	s.recent = append(s.recent, p)
	if len(s.recent) > keep {
		s.recent = s.recent[len(s.recent)-keep:]
	}
}

// newAttempt builds an ordinary shopper's payment.
func (s *Simulator) newAttempt(r *rand.Rand) attempt {
	m := s.merchants[r.IntN(len(s.merchants))]
	in := s.instruments[r.IntN(len(s.instruments))]

	currency := m.SettlementCurrency
	if r.Float64() < 0.10 { // cross-border purchases priced in USD
		currency = "USD"
	}
	amount, _ := money.RandomAmount(r, currency, false)

	ip := in.IssuingCountry
	if r.Float64() < 0.04 { // travellers / VPN users
		ip = pick(r, countries)
	}
	approval := 0.93
	if ip != in.IssuingCountry {
		approval = 0.70
	}
	if m.RiskTier == "high" {
		approval -= 0.10
	}
	return attempt{
		Payment: store.Payment{
			ID:             uuid.Must(uuid.NewV7()), // time-ordered: friendlier to B-tree indexes than v4
			MerchantID:     m.ID,
			CustomerID:     in.CustomerID,
			InstrumentID:   in.ID,
			IdempotencyKey: "ord_" + randHex(r, 8),
			AmountMinor:    amount,
			Currency:       currency,
			PSP:            pick(r, psps),
			IPCountry:      ip,
		},
		approvalRate: approval,
	}
}

// lifecycle walks one payment through created -> authorized/failed -> captured/voided
// -> maybe refunded, sleeping between steps like a real integration would.
func (s *Simulator) lifecycle(ctx context.Context, r *rand.Rand, a attempt) {
	log := s.log.With("payment_id", a.ID, "merchant_id", a.MerchantID)

	created, err := s.st.InsertPayment(ctx, a.Payment)
	if err != nil {
		log.Error("insert payment failed", "err", err)
		return
	}
	if !created {
		log.Warn("idempotency key already used, nothing created", "key", a.IdempotencyKey)
		return
	}
	metrics.PaymentsByStatus.WithLabelValues(string(payment.StatusCreated)).Inc()
	log.Debug("payment created", "amount", money.Format(a.AmountMinor, a.Currency))

	if !sleep(ctx, between(r, 50*time.Millisecond, 400*time.Millisecond)) { // network round-trip to the issuer
		return
	}
	if r.Float64() >= a.approvalRate {
		reason := payment.FailureReasons[r.IntN(len(payment.FailureReasons))]
		if a.fraud && r.Float64() < 0.6 {
			reason = "suspected_fraud"
		}
		s.move(ctx, log, a, payment.StatusCreated, payment.StatusFailed, reason)
		return
	}
	if !s.move(ctx, log, a, payment.StatusCreated, payment.StatusAuthorized, "") {
		return
	}

	if !sleep(ctx, between(r, 500*time.Millisecond, 3*time.Second)) { // merchant confirms the order
		return
	}
	if r.Float64() < 0.03 { // order cancelled before shipping
		s.move(ctx, log, a, payment.StatusAuthorized, payment.StatusVoided, "")
		return
	}
	if !s.move(ctx, log, a, payment.StatusAuthorized, payment.StatusCaptured, "") {
		return
	}

	if r.Float64() >= s.cfg.RefundRate || !sleep(ctx, between(r, 2*time.Second, 8*time.Second)) {
		return
	}
	refund := a.AmountMinor
	if r.Float64() < 0.4 { // partial refund: 10%..90%
		refund = max(1, a.AmountMinor*int64(10+r.IntN(81))/100)
	}
	s.refund(ctx, log, a, refund)
	if refund < a.AmountMinor && r.Float64() < 0.3 { // sometimes a 2nd partial that may exceed the total
		s.refund(ctx, log, a, a.AmountMinor-refund+int64(r.IntN(2)))
	}
}

func (s *Simulator) move(ctx context.Context, log *slog.Logger, a attempt, from, to payment.Status, reason string) bool {
	if err := s.st.Transition(ctx, a.ID, from, to, reason, s.cfg.FeeBps); err != nil {
		if ctx.Err() == nil {
			log.Error("transition failed", "from", from, "to", to, "err", err)
		}
		return false
	}
	metrics.PaymentsByStatus.WithLabelValues(string(to)).Inc()
	return true
}

func (s *Simulator) refund(ctx context.Context, log *slog.Logger, a attempt, amount int64) {
	_, err := s.st.Refund(ctx, a.ID, amount, "customer_request")
	switch {
	case errors.Is(err, store.ErrRefundNotAllowed):
		log.Info("refund rejected by business rule", "err", err) // expected sometimes: proves the guard works
	case err != nil:
		if ctx.Err() == nil {
			log.Error("refund failed", "err", err)
		}
	default:
		metrics.Refunds.Inc()
	}
}

// duplicate re-sends an earlier request with the same idempotency key, as a
// merchant would after a timeout. Correct behaviour: nothing new is created.
func (s *Simulator) duplicate(ctx context.Context, p store.Payment) {
	p.ID = uuid.Must(uuid.NewV7()) // a retry carries a new request, same key
	created, err := s.st.InsertPayment(ctx, p)
	if err != nil {
		s.log.Error("duplicate insert failed", "err", err)
		return
	}
	metrics.DuplicateRequests.Inc()
	if created {
		s.log.Error("IDEMPOTENCY BROKEN: duplicate request created a payment", "key", p.IdempotencyKey)
	}
}

// fraudBurst injects one of two attack patterns, launched as fast concurrent
// attempts on the same card. Phase 3's Flink rules must detect them.
//   - card_testing: many tiny amounts across many merchants, mostly declined
//   - ato_high_value: a few large amounts from a foreign IP (account takeover)
func (s *Simulator) fraudBurst(ctx context.Context, r *rand.Rand) {
	burstID := uuid.Must(uuid.NewV7())
	in := s.instruments[r.IntN(len(s.instruments))]
	foreign := in.IssuingCountry
	for foreign == in.IssuingCountry {
		foreign = pick(r, countries)
	}

	scenario, n, approval := "card_testing", 8+r.IntN(8), 0.20
	if r.Float64() < 0.3 {
		scenario, n, approval = "ato_high_value", 3+r.IntN(3), 0.60
	}
	metrics.FraudBursts.Inc()
	s.log.Info("fraud burst injected", "burst_id", burstID, "scenario", scenario, "attempts", n, "instrument_id", in.ID)

	var wg sync.WaitGroup
	for i := 0; i < n && ctx.Err() == nil; i++ {
		m := s.merchants[r.IntN(len(s.merchants))]
		var amount int64
		if scenario == "card_testing" {
			units, _ := money.MinorUnits(m.SettlementCurrency)
			amount = int64(1+r.IntN(5)) * pow10(units) // 1..5 major units: "does this card work?"
		} else {
			amount, _ = money.RandomAmount(r, m.SettlementCurrency, true)
		}
		a := attempt{
			Payment: store.Payment{
				ID: uuid.Must(uuid.NewV7()), MerchantID: m.ID, CustomerID: in.CustomerID, InstrumentID: in.ID,
				IdempotencyKey: "ord_" + randHex(r, 8), AmountMinor: amount, Currency: m.SettlementCurrency,
				PSP: pick(r, psps), IPCountry: foreign,
			},
			approvalRate: approval,
			fraud:        true,
		}
		if err := s.st.InsertFraudLabels(ctx, burstID, scenario, []uuid.UUID{a.ID}); err != nil && ctx.Err() == nil {
			s.log.Error("fraud label insert failed", "err", err)
		}
		child := rand.New(rand.NewPCG(r.Uint64(), r.Uint64()))
		wg.Add(1)
		go func() { defer wg.Done(); s.lifecycle(ctx, child, a) }()
		sleep(ctx, between(r, 300*time.Millisecond, 1500*time.Millisecond))
	}
	wg.Wait()
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// sleep waits for d or until ctx is cancelled; returns false if cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func between(r *rand.Rand, lo, hi time.Duration) time.Duration {
	return lo + time.Duration(r.Int64N(int64(hi-lo)))
}

func randHex(r *rand.Rand, nBytes int) string {
	b := make([]byte, nBytes)
	for i := range b {
		b[i] = byte(r.UintN(256))
	}
	return hex.EncodeToString(b)
}

func pow10(n int) int64 {
	p := int64(1)
	for i := 0; i < n; i++ {
		p *= 10
	}
	return p
}
