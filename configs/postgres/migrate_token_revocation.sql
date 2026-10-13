-- ================================================================
-- 新增令牌吊销 / 签发索引两张表（供已部署的库执行）
-- ================================================================
--
-- 新库不需要：configs/postgres/init.sql §3.16 已包含同样的定义。
--
-- 要解决的问题：auth_service 的吊销名单与"用户在用哪些令牌"索引都在进程内存里，
-- 于是重启后 RevokeAllOf(uid) 找不到任何令牌 —— 用户点"退出登录"只吊销了
-- access token，refresh token 仍可换出新票；多副本下另一副本也看不到吊销。
--
-- 用法：
--   docker compose exec -T postgres psql -U aegis -d aegis \
--     < configs/postgres/migrate_token_revocation.sql
--
-- ★ 这里用 IF NOT EXISTS 是安全的：两张都是普通表，不像分区那样存在
--   "名字判重但约束按父表判"的歧义（见 migrate_default_partitions.sql 的说明）。

\set ON_ERROR_STOP on

CREATE TABLE IF NOT EXISTS token_revocation (
  jti         VARCHAR(64)  PRIMARY KEY,
  uid         BIGINT       NOT NULL,
  expires_at  TIMESTAMPTZ  NOT NULL,
  revoked_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_token_revocation_exp ON token_revocation (expires_at);

CREATE TABLE IF NOT EXISTS issued_token (
  uid         BIGINT       NOT NULL,
  jti         VARCHAR(64)  NOT NULL,
  expires_at  TIMESTAMPTZ  NOT NULL,
  issued_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
  PRIMARY KEY (uid, jti)
);

CREATE INDEX IF NOT EXISTS idx_issued_token_uid_exp ON issued_token (uid, expires_at);
CREATE INDEX IF NOT EXISTS idx_issued_token_exp     ON issued_token (expires_at);

-- 校验两张表与索引就位
SELECT t.relname AS table_name,
       (SELECT count(*) FROM pg_indexes i WHERE i.tablename = t.relname) AS index_count
  FROM pg_class t
 WHERE t.relname IN ('token_revocation', 'issued_token')
 ORDER BY t.relname;
