-- Double-entry ledger + transactions + idempotency

-- Transactions record a business event (gift/topup/withdrawal).
CREATE TABLE IF NOT EXISTS transactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    idempotency_key TEXT UNIQUE,                          -- for safe retries
    kind TEXT NOT NULL CHECK (kind IN ('gift','topup','withdrawal')),
    amount BIGINT NOT NULL CHECK (amount > 0),            -- minor units (kobo)
    currency TEXT NOT NULL DEFAULT 'NGN',
    metadata JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Ledger entries: every transaction writes two rows (debit/credit).
CREATE TABLE IF NOT EXISTS ledger_entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tx_id UUID NOT NULL REFERENCES transactions(id) ON DELETE CASCADE,
    wallet_id UUID NOT NULL REFERENCES wallets(id) ON DELETE CASCADE,
    direction TEXT NOT NULL CHECK (direction IN ('debit','credit')),
    amount BIGINT NOT NULL CHECK (amount > 0),            -- minor units
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_ledger_tx ON ledger_entries(tx_id);
CREATE INDEX IF NOT EXISTS idx_ledger_wallet ON ledger_entries(wallet_id);
CREATE INDEX IF NOT EXISTS idx_tx_created_at ON transactions(created_at DESC);

-- (Optional) A view to compute balance from ledger; we will compute in code for now.
-- CREATE VIEW wallet_balance_from_ledger AS
-- SELECT wallet_id,
--   COALESCE(SUM(CASE WHEN direction='credit' THEN amount ELSE -amount END), 0)::BIGINT AS balance
-- FROM ledger_entries
-- GROUP BY wallet_id;
