DELETE FROM wallets WHERE user_id IN (SELECT id FROM users WHERE email='test@okies.app');
DELETE FROM users WHERE email='test@okies.app';
