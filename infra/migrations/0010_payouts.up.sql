-- payout destinations saved per user
CREATE TABLE IF NOT EXISTS payout_destinations (
  id              UUID        PRIMARY KEY,
  user_id         UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  bank_code       TEXT        NOT NULL,
  account_number  TEXT        NOT NULL,
  account_name    TEXT        NOT NULL,
  is_default      BOOLEAN     NOT NULL DEFAULT FALSE,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (user_id, bank_code, account_number)
);
CREATE INDEX IF NOT EXISTS idx_payout_destinations_user ON payout_destinations(user_id);

-- payouts (withdrawals) initiated by users
CREATE TABLE IF NOT EXISTS payouts (
  id             UUID        PRIMARY KEY,
  user_id        UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  destination_id UUID        NOT NULL REFERENCES payout_destinations(id) ON DELETE RESTRICT,
  amount         BIGINT      NOT NULL CHECK (amount > 0),
  currency       TEXT        NOT NULL DEFAULT 'NGN',
  status         TEXT        NOT NULL CHECK (status IN ('pending','processing','succeeded','failed','cancelled')),
  reference      TEXT        NOT NULL UNIQUE, -- Flutterwave transfer reference
  reason         TEXT,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_payouts_user ON payouts(user_id, created_at DESC);
