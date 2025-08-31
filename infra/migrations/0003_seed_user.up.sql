INSERT INTO users (email, password_hash, role, username, display_name)
VALUES ('test@okies.app', 'dev_only_hash', 'user', 'testuser', 'Test User')
ON CONFLICT (email) DO NOTHING;

INSERT INTO wallets (user_id, balance)
SELECT id, 0 FROM users WHERE email='test@okies.app'
ON CONFLICT DO NOTHING;
