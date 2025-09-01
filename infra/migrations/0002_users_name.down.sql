DROP INDEX IF EXISTS idx_users_username_lower;
ALTER TABLE users
  DROP COLUMN IF EXISTS display_name,
  DROP COLUMN IF EXISTS username;
