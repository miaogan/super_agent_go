-- ★ 历史增量脚本：仅用于升级「早于本功能的老库」。
--   新库一律由 configs/postgres/init.sql 一次建全，init.sql 已包含本文件的全部表。
--   因此日常改 schema 请改 init.sql（或对已有库写新的 migrate_*.sql），不要改本文件，
--   否则会出现两份定义不一致、改了一份不生效的情况。
--   挂载入口：docker-compose.yml 的 postgres 服务把 init.sql 挂到 /docker-entrypoint-initdb.d/。

-- ============================================================
-- migrate_agent_pg.sql — agent_service 接入 PG 存储的存量库迁移
--
-- 背景：
--   初版 DDL 中 agents/sessions/messages 用 VARCHAR(64) 主键，
--   且缺 tenant/avatar/tools/rag_*/citations 等列，与 proto 不一致。
--
-- ★ 前置条件：三张表必须为空（此前数据都在内存仓储里）。
--   若已有数据，请先备份，本脚本会 DROP 重建。
--
-- 用法：
--   docker cp configs/postgres/migrate_agent_pg.sql aegis-postgres:/tmp/
--   docker exec aegis-postgres psql -U aegis -d aegis -f /tmp/migrate_agent_pg.sql
-- ============================================================

DROP TABLE IF EXISTS messages CASCADE;
DROP TABLE IF EXISTS sessions CASCADE;
DROP TABLE IF EXISTS agents CASCADE;

-- ================================================================
-- agents — 智能体表
-- ================================================================
CREATE TABLE agents (
  agent_id       BIGSERIAL    PRIMARY KEY,
  uid            BIGINT       NOT NULL,
  tenant         VARCHAR(64)  NOT NULL DEFAULT '',
  name           VARCHAR(128) NOT NULL,
  description    VARCHAR(512) NOT NULL DEFAULT '',
  avatar         VARCHAR(512) NOT NULL DEFAULT '',
  system_prompt  TEXT         NOT NULL,
  model          VARCHAR(64)  NOT NULL DEFAULT '',
  temperature    NUMERIC(3,2) NOT NULL DEFAULT 0.70,
  max_tokens     INTEGER      NOT NULL DEFAULT 4096,
  context_rounds INTEGER      NOT NULL DEFAULT 20,
  tools          TEXT[]       NOT NULL DEFAULT '{}',
  kb_ids         BIGINT[]     NOT NULL DEFAULT '{}',
  rag_enabled    BOOLEAN      NOT NULL DEFAULT FALSE,
  rag_top_k      INTEGER      NOT NULL DEFAULT 4,
  rag_score_threshold REAL    NOT NULL DEFAULT 0.30,
  status         SMALLINT     NOT NULL DEFAULT 1,
  is_public      BOOLEAN      NOT NULL DEFAULT FALSE,
  created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
  deleted_at     TIMESTAMPTZ
);

CREATE INDEX idx_agents_uid    ON agents (uid);
CREATE INDEX idx_agents_status ON agents (status);
CREATE INDEX idx_agents_public ON agents (is_public, status);
CREATE INDEX idx_agents_kb_ids ON agents USING GIN (kb_ids);
-- Agent 名称在租户内唯一（对齐内存版 CreateAgent 的查重语义）；
-- 部分唯一索引：软删除后允许同名重建
CREATE UNIQUE INDEX uk_agents_tenant_name ON agents (tenant, name) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_agents_updated_at
  BEFORE UPDATE ON agents FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ================================================================
-- sessions — 会话表
-- ================================================================
CREATE TABLE sessions (
  session_id      BIGSERIAL   PRIMARY KEY,
  uid             BIGINT      NOT NULL,
  agent_id        BIGINT      NOT NULL DEFAULT 0,
  tenant          VARCHAR(64) NOT NULL DEFAULT '',
  title           VARCHAR(255) NOT NULL DEFAULT '',
  message_count   INTEGER     NOT NULL DEFAULT 0,
  total_tokens    BIGINT      NOT NULL DEFAULT 0,
  metadata        JSONB,
  last_message_at TIMESTAMPTZ,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at      TIMESTAMPTZ
);

CREATE INDEX idx_sessions_uid_time ON sessions (uid, last_message_at DESC);
CREATE INDEX idx_sessions_agent    ON sessions (agent_id);
CREATE INDEX idx_sessions_created  ON sessions (created_at);
CREATE TRIGGER trg_sessions_updated_at
  BEFORE UPDATE ON sessions FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ================================================================
-- messages — 消息表（月度分区）
-- ================================================================
CREATE TABLE messages (
  msg_id              BIGSERIAL   NOT NULL,
  session_id          BIGINT      NOT NULL,
  uid                 BIGINT      NOT NULL,
  role                VARCHAR(16) NOT NULL,
  content             TEXT        NOT NULL,
  tool_calls          JSONB,
  tool_call_id        VARCHAR(64) NOT NULL DEFAULT '',
  model               VARCHAR(64) NOT NULL DEFAULT '',
  prompt_tokens       INTEGER     NOT NULL DEFAULT 0,
  completion_tokens   INTEGER     NOT NULL DEFAULT 0,
  finish_reason       VARCHAR(32) NOT NULL DEFAULT '',
  latency_ms          INTEGER     NOT NULL DEFAULT 0,
  citations           JSONB,
  cited_chunk_ids     VARCHAR(64)[] NOT NULL DEFAULT '{}',
  degraded            BOOLEAN     NOT NULL DEFAULT FALSE,
  degrade_reason      VARCHAR(255) NOT NULL DEFAULT '',
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (msg_id, created_at)
) PARTITION BY RANGE (created_at);

CREATE TABLE messages_2026_10 PARTITION OF messages
  FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');
CREATE TABLE messages_2026_11 PARTITION OF messages
  FOR VALUES FROM ('2026-11-01') TO ('2026-12-01');
CREATE TABLE messages_2026_12 PARTITION OF messages
  FOR VALUES FROM ('2026-12-01') TO ('2027-01-01');

CREATE INDEX idx_messages_session_time ON messages (session_id, created_at DESC);
CREATE INDEX idx_messages_uid_time     ON messages (uid, created_at DESC);

-- ================================================================
-- 序列起点对齐内存版 ID 区间（agents≥1001 / sessions≥5001 / messages≥90001）
-- ================================================================
SELECT setval(pg_get_serial_sequence('agents','agent_id'),   1000,  true);
SELECT setval(pg_get_serial_sequence('sessions','session_id'), 5000, true);
SELECT setval(pg_get_serial_sequence('messages','msg_id'),   90000, true);
