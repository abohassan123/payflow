//go:build integration

// Integration tests run against a REAL PostgreSQL with the PayFlow schema.
// They are skipped by plain `go test ./...` (build tag) and run with:
//
//	TEST_DATABASE_URL=postgres://payflow_app:...@localhost:5432/payflow go test -tags integration ./...
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/payflow/simulator/internal/payment"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	st, err := New(context.Background(), url, 10*time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

// fixture creates one merchant, customer and card and returns a ready payment.
func fixture(t *testing.T, st *Store) Payment {
	t.Helper()
	ctx := context.Background()
	m, c, in := uuid.New(), uuid.New(), uuid.New()
	_, err := st.pool.Exec(ctx, `
		INSERT INTO merchants (merchant_id, legal_name, country_code, mcc, settlement_currency) VALUES ($1, 'Test', 'EG', '5411', 'EGP')`, m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `INSERT INTO customers (customer_id, email_sha256, country_code) VALUES ($1, $2, 'EG')`, c, hash(c)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `
		INSERT INTO payment_instruments (instrument_id, customer_id, card_brand, bin, last4, issuing_country, fingerprint)
		VALUES ($1, $2, 'visa', '411111', '1111', 'EG', $3)`, in, c, hash(in)); err != nil {
		t.Fatal(err)
	}
	return Payment{
		ID: uuid.Must(uuid.NewV7()), MerchantID: m, CustomerID: c, InstrumentID: in,
		IdempotencyKey: "test_" + uuid.NewString(), AmountMinor: 10000, Currency: "EGP", PSP: "adyen", IPCountry: "EG",
	}
}

func TestIdempotencyKeyPreventsDuplicates(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	p := fixture(t, st)

	created, err := st.InsertPayment(ctx, p)
	if err != nil || !created {
		t.Fatalf("first insert: created=%v err=%v", created, err)
	}
	p.ID = uuid.Must(uuid.NewV7()) // a retry: new request id, same key
	created, err = st.InsertPayment(ctx, p)
	if err != nil || created {
		t.Fatalf("retry must not create a payment: created=%v err=%v", created, err)
	}
}

func TestCaptureWritesBalancedLedger(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	p := fixture(t, st)
	mustInsert(t, st, p)

	if err := st.Transition(ctx, p.ID, payment.StatusCreated, payment.StatusAuthorized, "", 250); err != nil {
		t.Fatal(err)
	}
	if err := st.Transition(ctx, p.ID, payment.StatusAuthorized, payment.StatusCaptured, "", 250); err != nil {
		t.Fatal(err)
	}
	var debits, credits, fee int64
	err := st.pool.QueryRow(ctx, `
		SELECT sum(amount_minor) FILTER (WHERE direction = 'debit'),
		       sum(amount_minor) FILTER (WHERE direction = 'credit'),
		       sum(amount_minor) FILTER (WHERE account = 'fee_revenue')
		FROM ledger_entries WHERE payment_id = $1`, p.ID).Scan(&debits, &credits, &fee)
	if err != nil {
		t.Fatal(err)
	}
	if debits != 10000 || credits != 10000 || fee != 250 {
		t.Errorf("ledger: debits=%d credits=%d fee=%d, want 10000/10000/250", debits, credits, fee)
	}
}

func TestStaleTransitionIsRejected(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	p := fixture(t, st)
	mustInsert(t, st, p)

	if err := st.Transition(ctx, p.ID, payment.StatusCreated, payment.StatusFailed, "do_not_honor", 250); err != nil {
		t.Fatal(err)
	}
	// Another worker still believes the payment is "created":
	err := st.Transition(ctx, p.ID, payment.StatusCreated, payment.StatusAuthorized, "", 250)
	if !errors.Is(err, ErrStaleState) {
		t.Fatalf("want ErrStaleState, got %v", err)
	}
}

func TestConcurrentRefundsCannotExceedCapturedAmount(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	p := fixture(t, st)
	mustInsert(t, st, p)
	_ = st.Transition(ctx, p.ID, payment.StatusCreated, payment.StatusAuthorized, "", 250)
	_ = st.Transition(ctx, p.ID, payment.StatusAuthorized, payment.StatusCaptured, "", 250)

	// 10 goroutines each try to refund 40% at the same time. Only 2 may succeed.
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := st.Refund(ctx, p.ID, 4000, "test"); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			} else if !errors.Is(err, ErrRefundNotAllowed) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 2 {
		t.Fatalf("expected exactly 2 successful refunds, got %d", ok)
	}
}

func TestLedgerIsAppendOnly(t *testing.T) {
	st := newTestStore(t)
	_, err := st.pool.Exec(context.Background(), `UPDATE ledger_entries SET amount_minor = amount_minor WHERE false`)
	// The UPDATE privilege itself is granted, but the trigger blocks any row change.
	// With WHERE false no row fires the trigger, so try a real row if one exists:
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := st.pool.QueryRow(context.Background(), `SELECT entry_id FROM ledger_entries LIMIT 1`).Scan(&id); err != nil {
		t.Skip("no ledger rows yet")
	}
	_, err = st.pool.Exec(context.Background(), `UPDATE ledger_entries SET amount_minor = 1 WHERE entry_id = $1`, id)
	if err == nil {
		t.Fatal("ledger update must be blocked")
	}
}

func hash(id uuid.UUID) string {
	h := sha256.Sum256(id[:])
	return hex.EncodeToString(h[:])
}

func mustInsert(t *testing.T, st *Store, p Payment) {
	t.Helper()
	if created, err := st.InsertPayment(context.Background(), p); err != nil || !created {
		t.Fatalf("insert: created=%v err=%v", created, err)
	}
}
