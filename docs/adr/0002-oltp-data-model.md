# ADR 0002: OLTP data model for payments

- Status: accepted
- Date: 2026-11-08

## Context
The simulator needs a transactional model that behaves like a real PSP, so later phases (CDC, streaming, reconciliation)
have realistic data and realistic failure modes.

## Decision
1. **PostgreSQL 17**, one database `payflow`, schema `payments`, plus `simulation` for ground-truth labels.
2. **Money as `bigint` minor units** plus an ISO-4217 currency code. Never floating point.
3. **Mutable `payments` row with a status state machine** (`created -> authorized -> captured`, or `failed`/`voided`),
   enforced in code (`internal/payment`) and by CHECK constraints. CDC will turn each update into an event.
4. **Idempotency** via `UNIQUE (merchant_id, idempotency_key)` and `INSERT ... ON CONFLICT DO NOTHING`.
5. **Optimistic locking** on transitions (`WHERE status = <expected>`), pessimistic `SELECT ... FOR UPDATE` on refunds.
6. **Double-entry, append-only ledger** with a deferred constraint trigger that rejects unbalanced journals at COMMIT.
7. **PCI-minded card data**: BIN + last4 + fingerprint only. Customer e-mail stored as SHA-256 only.
8. **UUIDv7** keys for runtime rows (time-ordered, index friendly); seeded reference data uses deterministic v4.
9. **Least-privilege roles**: admin (DDL), `payflow_app` (DML, no DELETE), `payflow_readonly` (SELECT).

## Consequences
- Good: correctness guarantees live in the database, not only in application code.
- Good: realistic edge cases (duplicate requests, refund races, stuck payments) for downstream phases.
- Bad: triggers add a small write cost per ledger insert; acceptable at simulator volumes.
- Schema changes are applied by init scripts only on an empty volume; a migration tool (e.g. goose or Flyway) is a
  planned improvement (Phase 13).
