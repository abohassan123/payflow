// Package store is the only place that talks SQL. Keeping SQL in one package
// makes it easy to review queries, add indexes for them, and test them.
package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/payflow/simulator/internal/metrics"
	"github.com/payflow/simulator/internal/money"
	"github.com/payflow/simulator/internal/payment"
)

// ErrStaleState means the row was not in the status we expected: someone else
// moved it first. With optimistic locking we detect this instead of overwriting.
var ErrStaleState = errors.New("payment is not in the expected status")

// ErrRefundNotAllowed means the refund would break a business rule.
var ErrRefundNotAllowed = errors.New("refund not allowed")

type Store struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// New opens a connection pool and waits (up to waitFor) for the DB to accept
// connections - containers start in parallel, so the DB may still be booting.
func New(ctx context.Context, url string, waitFor time.Duration, log *slog.Logger) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	cfg.MaxConns = 20
	cfg.MinConns = 2
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.ConnConfig.RuntimeParams["application_name"] = "payflow-simulator"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	deadline := time.Now().Add(waitFor)
	for attempt := 1; ; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = pool.Ping(pingCtx)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			pool.Close()
			return nil, fmt.Errorf("database not reachable after %s: %w", waitFor, err)
		}
		log.Warn("database not ready, retrying", "attempt", attempt, "err", err)
		time.Sleep(2 * time.Second)
	}
	return &Store{pool: pool, log: log}, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// ---------------------------------------------------------------------------
// Retry policy
// ---------------------------------------------------------------------------

// isTransient decides whether an error is worth retrying. Only errors where
// re-running the SAME transaction is safe: deadlocks, serialization failures,
// dropped connections. Never retry constraint violations - they will fail again.
func isTransient(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "40001", pgErr.Code == "40P01": // serialization failure, deadlock
			return true
		case len(pgErr.Code) == 5 && pgErr.Code[:2] == "08": // connection exceptions
			return true
		case pgErr.Code == "57P01": // admin shutdown (e.g. DB restart)
			return true
		}
		return false
	}
	return pgconn.SafeToRetry(err) || pgconn.Timeout(err)
}

// withRetry runs fn up to 5 times with exponential backoff + jitter, and
// records latency and final errors as metrics.
func (s *Store) withRetry(ctx context.Context, op string, fn func(context.Context) error) error {
	start := time.Now()
	defer func() { metrics.DBLatency.WithLabelValues(op).Observe(time.Since(start).Seconds()) }()

	backoff := 50 * time.Millisecond
	var err error
	for attempt := 1; attempt <= 5; attempt++ {
		if err = fn(ctx); err == nil || !isTransient(err) {
			break
		}
		s.log.Warn("transient db error, retrying", "op", op, "attempt", attempt, "err", err)
		jitter := time.Duration(rand.Int64N(int64(backoff)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff + jitter):
		}
		backoff *= 2
	}
	if err != nil && !errors.Is(err, ErrStaleState) && !errors.Is(err, ErrRefundNotAllowed) && ctx.Err() == nil {
		metrics.DBErrors.WithLabelValues(op).Inc()
	}
	return err
}

// ---------------------------------------------------------------------------
// Reference data (merchants, customers, instruments)
// ---------------------------------------------------------------------------

type Merchant struct {
	ID                 uuid.UUID
	LegalName          string
	CountryCode        string
	MCC                string
	SettlementCurrency string
	RiskTier           string
}

type Customer struct {
	ID          uuid.UUID
	EmailSHA256 string
	CountryCode string
}

type Instrument struct {
	ID             uuid.UUID
	CustomerID     uuid.UUID
	CardBrand      string
	BIN            string
	Last4          string
	IssuingCountry string
	Fingerprint    string
}

// SeedIfEmpty bulk-loads reference data with COPY (much faster than INSERTs)
// inside one transaction. An advisory lock makes it safe if two simulators
// start at once: the second waits, then sees data and skips.
func (s *Store) SeedIfEmpty(ctx context.Context, ms []Merchant, cs []Customer, is []Instrument) (bool, error) {
	seeded := false
	err := s.withRetry(ctx, "seed", func(ctx context.Context) error {
		return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(424242)"); err != nil {
				return err
			}
			var exists bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM merchants)").Scan(&exists); err != nil {
				return err
			}
			if exists {
				return nil
			}
			if _, err := tx.CopyFrom(ctx, pgx.Identifier{"merchants"},
				[]string{"merchant_id", "legal_name", "country_code", "mcc", "settlement_currency", "risk_tier"},
				pgx.CopyFromSlice(len(ms), func(i int) ([]any, error) {
					m := ms[i]
					return []any{m.ID, m.LegalName, m.CountryCode, m.MCC, m.SettlementCurrency, m.RiskTier}, nil
				})); err != nil {
				return fmt.Errorf("copy merchants: %w", err)
			}
			if _, err := tx.CopyFrom(ctx, pgx.Identifier{"customers"},
				[]string{"customer_id", "email_sha256", "country_code"},
				pgx.CopyFromSlice(len(cs), func(i int) ([]any, error) {
					c := cs[i]
					return []any{c.ID, c.EmailSHA256, c.CountryCode}, nil
				})); err != nil {
				return fmt.Errorf("copy customers: %w", err)
			}
			if _, err := tx.CopyFrom(ctx, pgx.Identifier{"payment_instruments"},
				[]string{"instrument_id", "customer_id", "card_brand", "bin", "last4", "issuing_country", "fingerprint"},
				pgx.CopyFromSlice(len(is), func(i int) ([]any, error) {
					in := is[i]
					return []any{in.ID, in.CustomerID, in.CardBrand, in.BIN, in.Last4, in.IssuingCountry, in.Fingerprint}, nil
				})); err != nil {
				return fmt.Errorf("copy instruments: %w", err)
			}
			seeded = true
			return nil
		})
	})
	return seeded, err
}

// LoadMerchants and LoadInstruments read reference data back, so a restarted
// simulator keeps using the same merchants and cards.
func (s *Store) LoadMerchants(ctx context.Context) ([]Merchant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT merchant_id, legal_name, country_code, mcc, settlement_currency, risk_tier
		FROM merchants ORDER BY merchant_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Merchant, error) {
		var m Merchant
		err := r.Scan(&m.ID, &m.LegalName, &m.CountryCode, &m.MCC, &m.SettlementCurrency, &m.RiskTier)
		return m, err
	})
}

func (s *Store) LoadInstruments(ctx context.Context) ([]Instrument, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT instrument_id, customer_id, card_brand, bin, last4, issuing_country, fingerprint
		FROM payment_instruments ORDER BY instrument_id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Instrument, error) {
		var in Instrument
		err := r.Scan(&in.ID, &in.CustomerID, &in.CardBrand, &in.BIN, &in.Last4, &in.IssuingCountry, &in.Fingerprint)
		return in, err
	})
}

// ---------------------------------------------------------------------------
// Payments
// ---------------------------------------------------------------------------

type Payment struct {
	ID             uuid.UUID
	MerchantID     uuid.UUID
	CustomerID     uuid.UUID
	InstrumentID   uuid.UUID
	IdempotencyKey string
	AmountMinor    int64
	Currency       string
	PSP            string
	IPCountry      string
}

// InsertPayment creates a payment in status "created". If the merchant already
// sent this idempotency key, nothing is inserted and created=false: that is how
// a PSP makes client retries safe.
func (s *Store) InsertPayment(ctx context.Context, p Payment) (created bool, err error) {
	err = s.withRetry(ctx, "insert_payment", func(ctx context.Context) error {
		tag, err := s.pool.Exec(ctx, `
			INSERT INTO payments (payment_id, merchant_id, customer_id, instrument_id, idempotency_key,
			                      amount_minor, currency, status, psp, ip_country)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'created', $8, $9)
			ON CONFLICT (merchant_id, idempotency_key) DO NOTHING`,
			p.ID, p.MerchantID, p.CustomerID, p.InstrumentID, p.IdempotencyKey,
			p.AmountMinor, p.Currency, p.PSP, p.IPCountry)
		created = err == nil && tag.RowsAffected() == 1
		return err
	})
	return created, err
}

// Transition moves a payment from one status to another, atomically with any
// ledger entries the move requires. The WHERE status = $from clause is the
// optimistic lock: if the row moved meanwhile, 0 rows update -> ErrStaleState.
func (s *Store) Transition(ctx context.Context, id uuid.UUID, from, to payment.Status, failureReason string, feeBps int64) error {
	if !payment.CanTransition(from, to) {
		return payment.ErrInvalidTransition{From: from, To: to}
	}
	return s.withRetry(ctx, "transition_"+string(to), func(ctx context.Context) error {
		return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			var amount int64
			var currency string
			err := tx.QueryRow(ctx, `
				UPDATE payments
				   SET status = $3, failure_reason = NULLIF($4, ''), version = version + 1
				 WHERE payment_id = $1 AND status = $2
				RETURNING amount_minor, currency`,
				id, string(from), string(to), failureReason).Scan(&amount, &currency)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrStaleState
			}
			if err != nil {
				return err
			}
			if to != payment.StatusCaptured {
				return nil
			}
			fee := money.Fee(amount, feeBps)
			entries := []entry{{"psp_receivable", "debit", amount}}
			if fee > 0 {
				entries = append(entries, entry{"fee_revenue", "credit", fee})
			}
			entries = append(entries, entry{"merchant_payable", "credit", amount - fee})
			return insertJournal(ctx, tx, id, nil, currency, entries)
		})
	})
}

// Refund refunds part or all of a captured payment. SELECT ... FOR UPDATE locks
// the payment row, so two concurrent refunds cannot both pass the "not more
// than captured" check (a classic race condition).
func (s *Store) Refund(ctx context.Context, paymentID uuid.UUID, amount int64, reason string) (uuid.UUID, error) {
	refundID := uuid.Must(uuid.NewV7())
	err := s.withRetry(ctx, "refund", func(ctx context.Context) error {
		return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			var status, currency string
			var captured, refunded int64
			err := tx.QueryRow(ctx, `
				SELECT status, currency, amount_minor, refunded_amount_minor
				  FROM payments WHERE payment_id = $1 FOR UPDATE`, paymentID).
				Scan(&status, &currency, &captured, &refunded)
			if err != nil {
				return err
			}
			if status != string(payment.StatusCaptured) {
				return fmt.Errorf("%w: payment is %s", ErrRefundNotAllowed, status)
			}
			if amount <= 0 || refunded+amount > captured {
				return fmt.Errorf("%w: %d + %d exceeds captured %d", ErrRefundNotAllowed, refunded, amount, captured)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO refunds (refund_id, payment_id, amount_minor, currency, reason, status)
				VALUES ($1, $2, $3, $4, $5, 'succeeded')`,
				refundID, paymentID, amount, currency, reason); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE payments SET refunded_amount_minor = refunded_amount_minor + $2, version = version + 1
				 WHERE payment_id = $1`, paymentID, amount); err != nil {
				return err
			}
			return insertJournal(ctx, tx, paymentID, &refundID, currency, []entry{
				{"merchant_payable", "debit", amount},
				{"psp_receivable", "credit", amount},
			})
		})
	})
	return refundID, err
}

// InsertFraudLabels records simulation ground truth (see simulation schema).
func (s *Store) InsertFraudLabels(ctx context.Context, burstID uuid.UUID, scenario string, paymentIDs []uuid.UUID) error {
	return s.withRetry(ctx, "fraud_labels", func(ctx context.Context) error {
		_, err := s.pool.Exec(ctx, `
			INSERT INTO simulation.fraud_labels (payment_id, scenario, burst_id)
			SELECT unnest($1::uuid[]), $2, $3
			ON CONFLICT (payment_id) DO NOTHING`, paymentIDs, scenario, burstID)
		return err
	})
}

// ---------------------------------------------------------------------------
// Ledger
// ---------------------------------------------------------------------------

type entry struct {
	account   string
	direction string
	amount    int64
}

// insertJournal writes one balanced journal. The DB's deferred constraint
// trigger re-checks the balance at COMMIT, so a bug here cannot corrupt the books.
func insertJournal(ctx context.Context, tx pgx.Tx, paymentID uuid.UUID, refundID *uuid.UUID, currency string, entries []entry) error {
	journalID := uuid.Must(uuid.NewV7())
	batch := &pgx.Batch{}
	for _, e := range entries {
		batch.Queue(`
			INSERT INTO ledger_entries (journal_id, payment_id, refund_id, account, direction, amount_minor, currency)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			journalID, paymentID, refundID, e.account, e.direction, e.amount, currency)
	}
	return tx.SendBatch(ctx, batch).Close()
}
