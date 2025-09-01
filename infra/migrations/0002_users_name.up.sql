-- Add optional profile fields to users
ALTER TABLE users
  ADD COLUMN IF NOT EXISTS username TEXT UNIQUE,
  ADD COLUMN IF NOT EXISTS display_name TEXT;

-- Helpful index for lookup by username (case-insensitive)
CREATE INDEX IF NOT EXISTS idx_users_username_lower ON users (lower(username));
