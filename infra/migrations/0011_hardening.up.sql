-- Ensure idempotency keys cannot duplicate writes
CREATE UNIQUE INDEX IF NOT EXISTS uq_transactions_idem ON transactions (idempotency_key);

-- Common helpful indexes
CREATE INDEX IF NOT EXISTS ix_users_email ON users (email);
CREATE UNIQUE INDEX IF NOT EXISTS uq_users_email ON users (email);
CREATE INDEX IF NOT EXISTS ix_wallets_user_id ON wallets (user_id);
CREATE INDEX IF NOT EXISTS ix_ledger_tx_id ON ledger_entries (tx_id);
CREATE INDEX IF NOT EXISTS ix_withdrawals_status ON withdrawals (status);