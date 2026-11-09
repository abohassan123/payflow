-- =============================================================================
-- Phase 1 validation. Run with:  make psql   then   \i /queries/validate_phase1.sql
-- Every check prints a result you can compare with the "expected" comment.
-- =============================================================================
SET search_path TO payments;
\echo '--- 1. Row counts (expected: 50 merchants, 2000 customers, ~3000 instruments, payments growing)'
SELECT 'merchants' AS table_name, count(*) FROM merchants
UNION ALL SELECT 'customers', count(*) FROM customers
UNION ALL SELECT 'payment_instruments', count(*) FROM payment_instruments
UNION ALL SELECT 'payments', count(*) FROM payments
UNION ALL SELECT 'refunds', count(*) FROM refunds
UNION ALL SELECT 'ledger_entries', count(*) FROM ledger_entries;

\echo '--- 2. Status mix (expected: ~75-85% captured, ~10-20% failed - more with a higher SIM_FRAUD_RATE - ~2% voided; created/authorized only while payments are in flight)'
SELECT status, count(*), round(100.0 * count(*) / sum(count(*)) OVER (), 1) AS pct
FROM payments GROUP BY status ORDER BY count(*) DESC;

\echo '--- 3. Ledger balance per currency (expected: debit_minus_credit = 0 on every row)'
SELECT currency,
       sum(CASE direction WHEN 'debit' THEN amount_minor ELSE 0 END)  AS debits,
       sum(CASE direction WHEN 'credit' THEN amount_minor ELSE 0 END) AS credits,
       sum(CASE direction WHEN 'debit' THEN amount_minor ELSE -amount_minor END) AS debit_minus_credit
FROM ledger_entries GROUP BY currency ORDER BY currency;

\echo '--- 4. Every captured payment has exactly one capture journal (expected: 0 rows)'
SELECT p.payment_id
FROM payments p
LEFT JOIN ledger_entries l ON l.payment_id = p.payment_id AND l.refund_id IS NULL
WHERE p.status = 'captured'
GROUP BY p.payment_id
HAVING count(DISTINCT l.journal_id) <> 1;

\echo '--- 5. Refund totals match payments.refunded_amount_minor (expected: 0 rows)'
SELECT p.payment_id, p.refunded_amount_minor, COALESCE(sum(r.amount_minor), 0) AS refunds_sum
FROM payments p LEFT JOIN refunds r ON r.payment_id = p.payment_id AND r.status = 'succeeded'
GROUP BY p.payment_id, p.refunded_amount_minor
HAVING p.refunded_amount_minor <> COALESCE(sum(r.amount_minor), 0);

\echo '--- 6. Idempotency: no duplicate (merchant, key) pairs (expected: 0 rows)'
SELECT merchant_id, idempotency_key, count(*) FROM payments
GROUP BY 1, 2 HAVING count(*) > 1;

\echo '--- 7. Approval rate by card-vs-IP country match (expected: mismatched is clearly lower)'
SELECT (pi.issuing_country = p.ip_country) AS country_match,
       count(*) AS attempts,
       round(100.0 * avg((p.status IN ('authorized', 'captured', 'voided'))::int), 1) AS approval_pct
FROM payments p JOIN payment_instruments pi USING (instrument_id)
WHERE p.status <> 'created'
GROUP BY 1;

\echo '--- 8. Fraud ground truth: bursts injected (expected: > 0 after a few minutes)'
SELECT scenario, count(DISTINCT burst_id) AS bursts, count(*) AS payments
FROM simulation.fraud_labels GROUP BY scenario;

\echo '--- 9. Amounts look right per currency (format minor units back to major)'
SELECT p.currency, c.minor_units, count(*),
       round(avg(p.amount_minor) / power(10, c.minor_units)::numeric, 2) AS avg_major_amount
FROM payments p JOIN currencies c ON c.code = p.currency
GROUP BY 1, 2 ORDER BY 3 DESC;

\echo '--- 10. Does the planner use our index? (expected: Index Scan or Bitmap Index Scan on ix_payments_merchant_created - NOT Seq Scan on payments)'
EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
SELECT payment_id, amount_minor, status
FROM payments
WHERE merchant_id = (SELECT merchant_id FROM merchants LIMIT 1)
ORDER BY created_at DESC
LIMIT 20;

\echo '--- 11. Top 5 statements by total time (pg_stat_statements)'
SELECT round(total_exec_time::numeric, 1) AS total_ms, calls,
       round(mean_exec_time::numeric, 3) AS mean_ms, left(query, 80) AS query
FROM public.pg_stat_statements
ORDER BY total_exec_time DESC LIMIT 5;
