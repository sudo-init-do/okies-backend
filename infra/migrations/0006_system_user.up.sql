-- System user + wallet (treasury) for platform debits/credits

-- Ensure UUIDs
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- Create system user if missing
DO $$
DECLARE sys_id UUID;
BEGIN
  SELECT id INTO sys_id FROM users WHERE email = 'system@okies.local';
  IF sys_id IS NULL THEN
    INSERT INTO users (email, password_hash, role, username, display_name)
    VALUES ('system@okies.local', '', 'admin', 'system', 'System Account')
    RETURNING id INTO sys_id;
  END IF;

  -- Create wallet for system user if missing
  IF NOT EXISTS (SELECT 1 FROM wallets WHERE user_id = sys_id) THEN
    INSERT INTO wallets (user_id, balance) VALUES (sys_id, 0);
  END IF;
END$$;
