-- Remove system wallet + user
DO $$
DECLARE sys_id UUID;
BEGIN
  SELECT id INTO sys_id FROM users WHERE email = 'system@okies.local';
  IF sys_id IS NOT NULL THEN
    DELETE FROM wallets WHERE user_id = sys_id;
    DELETE FROM users WHERE id = sys_id;
  END IF;
END$$;
