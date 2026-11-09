-- =============================================================================
-- PayFlow OLTP schema (PostgreSQL 17)
-- Runs once, automatically, the first time the postgres container starts with
-- an empty data volume (files in /docker-entrypoint-initdb.d run in name order).
-- =============================================================================

CREATE EXTENSION IF NOT EXISTS pg_stat_statements;  -- query performance stats

CREATE SCHEMA IF NOT EXISTS payments;    -- the "product" tables
CREATE SCHEMA IF NOT EXISTS simulation;  -- ground-truth labels, NOT production data

SET search_path TO payments;

-- -----------------------------------------------------------------------------
-- Shared trigger: keep updated_at honest without trusting the application.
-- CDC (Phase 2) and incremental dbt models (Phase 5) both rely on it.
-- -----------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION payments.set_updated_at() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$;

-- -----------------------------------------------------------------------------
-- Reference data: currencies and their minor units.
-- Money is ALWAYS stored as an integer number of minor units (piasters, cents,
-- fils). 12.50 EGP = 1250. KWD has 3 decimals, JPY has 0.
-- -----------------------------------------------------------------------------
CREATE TABLE currencies (
    code         char(3)  PRIMARY KEY,
    minor_units  smallint NOT NULL CHECK (minor_units BETWEEN 0 AND 4),
    name         text     NOT NULL
);

INSERT INTO currencies (code, minor_units, name) VALUES
    ('EGP', 2, 'Egyptian Pound'),
    ('USD', 2, 'US Dollar'),
    ('EUR', 2, 'Euro'),
    ('SAR', 2, 'Saudi Riyal'),
    ('AED', 2, 'UAE Dirham'),
    ('KWD', 3, 'Kuwaiti Dinar'),
    ('JPY', 0, 'Japanese Yen');

-- -----------------------------------------------------------------------------
-- Merchants: the businesses that accept payments through PayFlow.
-- -----------------------------------------------------------------------------
CREATE TABLE merchants (
    merchant_id          uuid        PRIMARY KEY,
    legal_name           text        NOT NULL,
    country_code         char(2)     NOT NULL,
    mcc                  char(4)     NOT NULL,          -- merchant category code
    settlement_currency  char(3)     NOT NULL REFERENCES currencies (code),
    risk_tier            text        NOT NULL DEFAULT 'standard'
                                     CHECK (risk_tier IN ('low', 'standard', 'high')),
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now()
);

-- -----------------------------------------------------------------------------
-- Customers: we never store raw e-mails. A SHA-256 hash is enough to join on.
-- -----------------------------------------------------------------------------
CREATE TABLE customers (
    customer_id   uuid        PRIMARY KEY,
    email_sha256  char(64)    NOT NULL UNIQUE,
    country_code  char(2)     NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- -----------------------------------------------------------------------------
-- Payment instruments (cards). PCI-DSS: never store the full card number (PAN)
-- or CVV. BIN (first 6) + last4 + a fingerprint is what analytics needs.
-- -----------------------------------------------------------------------------
CREATE TABLE payment_instruments (
    instrument_id    uuid        PRIMARY KEY,
    customer_id      uuid        NOT NULL REFERENCES customers (customer_id),
    card_brand       text        NOT NULL CHECK (card_brand IN ('visa', 'mastercard', 'amex', 'meeza')),
    bin              char(6)     NOT NULL,
    last4            char(4)     NOT NULL,
    issuing_country  char(2)     NOT NULL,
    fingerprint      char(64)    NOT NULL UNIQUE,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_instruments_customer ON payment_instruments (customer_id);

-- -----------------------------------------------------------------------------
-- Payments: one row per payment, mutated as it moves through its lifecycle:
--   created -> authorized -> captured
--          \-> failed     \-> voided
-- `version` supports optimistic locking; (merchant_id, idempotency_key) makes
-- client retries safe: the same request twice never creates two payments.
-- -----------------------------------------------------------------------------
CREATE TABLE payments (
    payment_id             uuid        PRIMARY KEY,
    merchant_id            uuid        NOT NULL REFERENCES merchants (merchant_id),
    customer_id            uuid        NOT NULL REFERENCES customers (customer_id),
    instrument_id          uuid        NOT NULL REFERENCES payment_instruments (instrument_id),
    idempotency_key        text        NOT NULL,
    amount_minor           bigint      NOT NULL CHECK (amount_minor > 0),
    currency               char(3)     NOT NULL REFERENCES currencies (code),
    status                 text        NOT NULL
                                       CHECK (status IN ('created', 'authorized', 'captured', 'failed', 'voided')),
    failure_reason         text,
    psp                    text        NOT NULL,          -- which acquirer/PSP processed it
    ip_country             char(2)     NOT NULL,          -- where the shopper connected from
    refunded_amount_minor  bigint      NOT NULL DEFAULT 0 CHECK (refunded_amount_minor >= 0),
    version                integer     NOT NULL DEFAULT 1,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_payments_idempotency UNIQUE (merchant_id, idempotency_key),
    CONSTRAINT ck_refund_not_above_amount CHECK (refunded_amount_minor <= amount_minor),
    CONSTRAINT ck_failure_reason CHECK ((status = 'failed') = (failure_reason IS NOT NULL))
);
-- Indexes match real query patterns (see infra/postgres/queries/validate_phase1.sql)
CREATE INDEX ix_payments_merchant_created   ON payments (merchant_id, created_at DESC);
CREATE INDEX ix_payments_instrument_created ON payments (instrument_id, created_at DESC);
-- Partial index: only "in-flight" rows, so it stays tiny. Used to find stuck payments.
CREATE INDEX ix_payments_inflight ON payments (updated_at) WHERE status IN ('created', 'authorized');

-- -----------------------------------------------------------------------------
-- Refunds: a captured payment can be refunded fully or in several partials.
-- -----------------------------------------------------------------------------
CREATE TABLE refunds (
    refund_id     uuid        PRIMARY KEY,
    payment_id    uuid        NOT NULL REFERENCES payments (payment_id),
    amount_minor  bigint      NOT NULL CHECK (amount_minor > 0),
    currency      char(3)     NOT NULL REFERENCES currencies (code),
    reason        text        NOT NULL,
    status        text        NOT NULL CHECK (status IN ('pending', 'succeeded', 'failed')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_refunds_payment ON refunds (payment_id);

-- -----------------------------------------------------------------------------
-- Ledger: double-entry bookkeeping. Every money movement is a journal of >= 2
-- entries whose debits equal its credits. Entries are append-only.
--   capture: DR psp_receivable  amount | CR merchant_payable amount-fee | CR fee_revenue fee
--   refund : DR merchant_payable amount | CR psp_receivable  amount
-- -----------------------------------------------------------------------------
CREATE TABLE ledger_entries (
    entry_id      bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    journal_id    uuid        NOT NULL,
    payment_id    uuid        NOT NULL REFERENCES payments (payment_id),
    refund_id     uuid        REFERENCES refunds (refund_id),
    account       text        NOT NULL CHECK (account IN ('psp_receivable', 'merchant_payable', 'fee_revenue')),
    direction     text        NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount_minor  bigint      NOT NULL CHECK (amount_minor > 0),
    currency      char(3)     NOT NULL REFERENCES currencies (code),
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ix_ledger_payment ON ledger_entries (payment_id);
CREATE INDEX ix_ledger_journal ON ledger_entries (journal_id);

-- Append-only: block UPDATE and DELETE on the ledger.
CREATE OR REPLACE FUNCTION payments.ledger_is_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only (% blocked)', TG_OP;
END;
$$;
CREATE TRIGGER trg_ledger_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION payments.ledger_is_append_only();

-- Balanced journals: checked at COMMIT time (DEFERRABLE INITIALLY DEFERRED),
-- so a transaction can insert the entries one by one and is only judged at the end.
CREATE OR REPLACE FUNCTION payments.check_journal_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    imbalance bigint;
BEGIN
    SELECT COALESCE(SUM(CASE direction WHEN 'debit' THEN amount_minor ELSE -amount_minor END), 0)
      INTO imbalance
      FROM payments.ledger_entries
     WHERE journal_id = NEW.journal_id;
    IF imbalance <> 0 THEN
        RAISE EXCEPTION 'journal % is not balanced (debits - credits = %)', NEW.journal_id, imbalance;
    END IF;
    RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER trg_ledger_balanced
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION payments.check_journal_balanced();

-- updated_at triggers
CREATE TRIGGER trg_merchants_updated   BEFORE UPDATE ON merchants           FOR EACH ROW EXECUTE FUNCTION payments.set_updated_at();
CREATE TRIGGER trg_customers_updated   BEFORE UPDATE ON customers           FOR EACH ROW EXECUTE FUNCTION payments.set_updated_at();
CREATE TRIGGER trg_instruments_updated BEFORE UPDATE ON payment_instruments FOR EACH ROW EXECUTE FUNCTION payments.set_updated_at();
CREATE TRIGGER trg_payments_updated    BEFORE UPDATE ON payments            FOR EACH ROW EXECUTE FUNCTION payments.set_updated_at();
CREATE TRIGGER trg_refunds_updated     BEFORE UPDATE ON refunds             FOR EACH ROW EXECUTE FUNCTION payments.set_updated_at();

-- -----------------------------------------------------------------------------
-- Simulation ground truth. A real PSP never knows this; we keep it so that in
-- Phase 3 we can measure how well the fraud rules catch the injected attacks.
-- -----------------------------------------------------------------------------
CREATE TABLE simulation.fraud_labels (
    payment_id  uuid        PRIMARY KEY,
    scenario    text        NOT NULL,
    burst_id    uuid        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);
