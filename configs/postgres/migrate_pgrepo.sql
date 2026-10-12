-- 对已有 DB 的增量 migration（不丢 demo 数据）

-- 1) users.uid 加 BIGSERIAL 行为（已有行 uid 保留）
ALTER TABLE users ALTER COLUMN uid DROP DEFAULT;
CREATE SEQUENCE IF NOT EXISTS users_uid_seq START WITH 1001;
ALTER TABLE users ALTER COLUMN uid SET DEFAULT nextval('users_uid_seq');
SELECT setval('users_uid_seq', COALESCE((SELECT MAX(uid) FROM users), 1));

-- 2) user_preferences 表
CREATE TABLE IF NOT EXISTS user_preferences (
  uid         BIGINT PRIMARY KEY,
  settings    JSONB  NOT NULL DEFAULT '{}'::jsonb,
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT fk_pref_user FOREIGN KEY (uid) REFERENCES users(uid) ON DELETE CASCADE
);
CREATE TRIGGER IF NOT EXISTS trg_user_preferences_updated_at
  BEFORE UPDATE ON user_preferences FOR EACH ROW EXECUTE FUNCTION set_updated_at();
