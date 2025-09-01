-- Withdrawals: hold funds (user -> system), then approve/reject
CREATE TABLE IF NOT EXISTS withdrawals (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  amount BIGINT NOT NULL CHECK (amount > 0),
  currency TEXT NOT NULL DEFAULT 'NGN',
  status TEXT NOT NULL CHECK (status IN ('pending','approved','rejected')),
  tx_id UUID NOT NULL REFERENCES transactions(id) ON DELETE RESTRICT, -- hold txn
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  approved_by UUID NULL REFERENCES users(id),
  approved_at TIMESTAMPTZ NULL
);

CREATE INDEX IF NOT EXISTS idx_withdrawals_user ON withdrawals(user_id);
CREATE INDEX IF NOT EXISTS idx_withdrawals_status ON withdrawals(status);
