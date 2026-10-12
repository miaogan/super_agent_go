-- =====================================================================
-- Aegis PostgreSQL 初始化脚本
-- 版本：v1.0 (PostgreSQL 18)
-- 来源：docs/04-数据库设计.md §3 / §6 / §7 / §8
--
-- ★ 分区表按月创建；当前脚本建 2026 Q4 子分区，后续按需追加。
-- ★ updated_at 自动维护通过触发器实现（PG 无 ON UPDATE 语法）。
-- ★ chunks.content_trgm 索引依赖 pg_trgm 扩展。
-- =====================================================================

-- ---- 扩展 ----
CREATE EXTENSION IF NOT EXISTS pg_trgm;        -- 模糊匹配（chunks.content_trgm）
CREATE EXTENSION IF NOT EXISTS pgcrypto;       -- gen_random_uuid / digest
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;


-- ---- 通用触发器：updated_at 自动维护 ----
CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;


-- ================================================================
-- §3.1 users — 用户表
-- ================================================================
CREATE TABLE users (
  uid             BIGSERIAL    PRIMARY KEY,
  username        VARCHAR(32)  NOT NULL,
  password_hash   VARCHAR(128) NOT NULL,
  nickname        VARCHAR(64)  NOT NULL DEFAULT '',
  avatar          VARCHAR(512) NOT NULL DEFAULT '',
  email           VARCHAR(128) NOT NULL DEFAULT '',
  phone           VARCHAR(32)  NOT NULL DEFAULT '',
  status          SMALLINT     NOT NULL DEFAULT 1,
  dept_id         BIGINT       NOT NULL DEFAULT 0,
  last_login_at   TIMESTAMPTZ,
  last_login_ip   VARCHAR(64)  NOT NULL DEFAULT '',
  created_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ  NOT NULL DEFAULT now(),
  deleted_at      TIMESTAMPTZ,
  source          VARCHAR(16)  NOT NULL DEFAULT 'local',
  tenant          VARCHAR(32)  NOT NULL DEFAULT 'default',
  CONSTRAINT uk_users_username UNIQUE (username),
  CONSTRAINT uk_users_email    UNIQUE (email)
);

CREATE INDEX idx_users_phone   ON users (phone);
CREATE INDEX idx_users_dept    ON users (dept_id);
CREATE INDEX idx_users_created ON users (created_at);
CREATE INDEX idx_users_alive   ON users (uid) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_users_updated_at
  BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- ================================================================
-- §3.2 roles — 角色表
-- ================================================================
CREATE TABLE roles (
  role_id     BIGINT       PRIMARY KEY,
  name        VARCHAR(64)  NOT NULL,
  description VARCHAR(255) NOT NULL DEFAULT '',
  is_system   BOOLEAN      NOT NULL DEFAULT FALSE,
  created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
  CONSTRAINT uk_roles_name UNIQUE (name)
);

CREATE TRIGGER trg_roles_updated_at
  BEFORE UPDATE ON roles FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- ================================================================
-- §3.3 user_roles — 用户角色关联
-- ================================================================
CREATE TABLE user_roles (
  id         BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  uid        BIGINT      NOT NULL,
  role_id    BIGINT      NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT uk_user_roles_uid_role UNIQUE (uid, role_id)
);

CREATE INDEX idx_user_roles_role ON user_roles (role_id);


-- ================================================================
-- §3.4 permissions / role_permissions
-- ================================================================
CREATE TABLE permissions (
  perm_id    BIGINT       PRIMARY KEY,
  code       VARCHAR(64)  NOT NULL,
  name       VARCHAR(64)  NOT NULL,
  resource   VARCHAR(64)  NOT NULL DEFAULT '',
  action     VARCHAR(32)  NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
  CONSTRAINT uk_permissions_code UNIQUE (code)
);

CREATE TABLE role_permissions (
  id         BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  role_id    BIGINT      NOT NULL,
  perm_id    BIGINT      NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT uk_role_permissions_role_perm UNIQUE (role_id, perm_id)
);


-- ================================================================
-- §3.5 departments — 部门表
-- ================================================================
CREATE TABLE departments (
  dept_id    BIGINT       PRIMARY KEY,
  name       VARCHAR(64)  NOT NULL,
  parent_id  BIGINT       NOT NULL DEFAULT 0,
  path       VARCHAR(255) NOT NULL DEFAULT '',
  sort       INTEGER      NOT NULL DEFAULT 0,
  created_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ  NOT NULL DEFAULT now(),
  deleted_at TIMESTAMPTZ
);

CREATE INDEX idx_departments_parent ON departments (parent_id);
CREATE INDEX idx_departments_path   ON departments (path);
CREATE TRIGGER trg_departments_updated_at
  BEFORE UPDATE ON departments FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- ================================================================
-- §3.6 api_keys
-- ================================================================
CREATE TABLE api_keys (
  key_id        BIGINT       PRIMARY KEY,
  uid           BIGINT       NOT NULL,
  app_id        VARCHAR(64)  NOT NULL,
  name          VARCHAR(128) NOT NULL DEFAULT '',
  key_hash      CHAR(64)     NOT NULL,
  key_prefix    VARCHAR(16)  NOT NULL DEFAULT '',
  status        SMALLINT     NOT NULL DEFAULT 1,
  expires_at    TIMESTAMPTZ,
  last_used_at  TIMESTAMPTZ,
  created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
  CONSTRAINT uk_api_keys_hash UNIQUE (key_hash)
);

CREATE INDEX idx_api_keys_uid ON api_keys (uid);
CREATE INDEX idx_api_keys_app ON api_keys (app_id);


-- ================================================================
-- §3.7 agents — 智能体表
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
-- §3.8 tools / agent_tools
-- ================================================================
CREATE TABLE tools (
  tool_id       BIGINT       PRIMARY KEY,
  name          VARCHAR(64)  NOT NULL,
  description   VARCHAR(512) NOT NULL,
  type          VARCHAR(32)  NOT NULL DEFAULT 'http',
  endpoint      VARCHAR(512) NOT NULL DEFAULT '',
  method        VARCHAR(16)  NOT NULL DEFAULT 'POST',
  params_schema JSONB,
  timeout_ms    INTEGER      NOT NULL DEFAULT 5000,
  status        SMALLINT     NOT NULL DEFAULT 1,
  created_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
  updated_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
  CONSTRAINT uk_tools_name UNIQUE (name)
);

CREATE INDEX idx_tools_params_gin ON tools USING GIN (params_schema jsonb_path_ops);
CREATE TRIGGER trg_tools_updated_at
  BEFORE UPDATE ON tools FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE agent_tools (
  id         BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  agent_id   VARCHAR(64) NOT NULL,
  tool_name  VARCHAR(64) NOT NULL,
  enabled    BOOLEAN     NOT NULL DEFAULT TRUE,
  config     JSONB,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT uk_agent_tools UNIQUE (agent_id, tool_name)
);

CREATE INDEX idx_agent_tools_tool ON agent_tools (tool_name);


-- ================================================================
-- §3.9 sessions — 会话表
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
-- §3.10 messages — 消息表（月度分区）
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
-- §3.11 token_usage — Token 用量明细（月度分区）
-- ================================================================
CREATE TABLE token_usage (
  id                BIGINT      GENERATED ALWAYS AS IDENTITY,
  uid               BIGINT      NOT NULL,
  app_id            VARCHAR(64) NOT NULL DEFAULT '',
  session_id        VARCHAR(64) NOT NULL DEFAULT '',
  agent_id          VARCHAR(64) NOT NULL DEFAULT '',
  model             VARCHAR(64) NOT NULL,
  provider          VARCHAR(32) NOT NULL DEFAULT '',
  prompt_tokens     INTEGER     NOT NULL DEFAULT 0,
  completion_tokens INTEGER     NOT NULL DEFAULT 0,
  total_tokens      INTEGER     NOT NULL DEFAULT 0,
  cost              NUMERIC(16,6) NOT NULL DEFAULT 0,
  is_fallback       BOOLEAN     NOT NULL DEFAULT FALSE,
  latency_ms        INTEGER     NOT NULL DEFAULT 0,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

CREATE TABLE token_usage_2026_10 PARTITION OF token_usage
  FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');
CREATE TABLE token_usage_2026_11 PARTITION OF token_usage
  FOR VALUES FROM ('2026-11-01') TO ('2026-12-01');
CREATE TABLE token_usage_2026_12 PARTITION OF token_usage
  FOR VALUES FROM ('2026-12-01') TO ('2027-01-01');

CREATE INDEX idx_token_usage_uid_time   ON token_usage (uid, created_at DESC);
CREATE INDEX idx_token_usage_app_time   ON token_usage (app_id, created_at DESC);
CREATE INDEX idx_token_usage_model_time ON token_usage (model, created_at DESC);
CREATE INDEX idx_token_usage_session    ON token_usage (session_id);


-- ================================================================
-- §3.12 token_usage_daily — 日聚合
-- ================================================================
CREATE TABLE token_usage_daily (
  id                BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  stat_date         DATE        NOT NULL,
  uid               BIGINT      NOT NULL DEFAULT 0,
  app_id            VARCHAR(64) NOT NULL DEFAULT '',
  model             VARCHAR(64) NOT NULL DEFAULT '',
  request_count     INTEGER     NOT NULL DEFAULT 0,
  prompt_tokens     BIGINT      NOT NULL DEFAULT 0,
  completion_tokens BIGINT      NOT NULL DEFAULT 0,
  total_tokens      BIGINT      NOT NULL DEFAULT 0,
  cost              NUMERIC(16,6) NOT NULL DEFAULT 0,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT uk_token_daily UNIQUE (stat_date, uid, app_id, model)
);

CREATE INDEX idx_token_daily_date_app ON token_usage_daily (stat_date, app_id);


-- ================================================================
-- §3.13 quotas — 配额表
-- ================================================================
CREATE TABLE quotas (
  id           BIGINT      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  subject_type VARCHAR(16) NOT NULL,
  subject_id   VARCHAR(64) NOT NULL,
  quota_total  BIGINT      NOT NULL DEFAULT 0,
  quota_used   BIGINT      NOT NULL DEFAULT 0,
  period       VARCHAR(16) NOT NULL DEFAULT 'month',
  reset_at     TIMESTAMPTZ,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT uk_quotas_subject UNIQUE (subject_type, subject_id)
);

CREATE INDEX idx_quotas_reset ON quotas (reset_at);
CREATE TRIGGER trg_quotas_updated_at
  BEFORE UPDATE ON quotas FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- ================================================================
-- §3.14 prompt_templates
-- ================================================================
CREATE TABLE prompt_templates (
  template_id VARCHAR(64)  PRIMARY KEY,
  uid         BIGINT       NOT NULL DEFAULT 0,
  name        VARCHAR(128) NOT NULL,
  content     TEXT         NOT NULL,
  variables   JSONB,
  version     INTEGER      NOT NULL DEFAULT 1,
  is_builtin  BOOLEAN      NOT NULL DEFAULT FALSE,
  created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
  deleted_at  TIMESTAMPTZ
);

CREATE INDEX idx_prompt_templates_uid ON prompt_templates (uid);
CREATE TRIGGER trg_prompt_templates_updated_at
  BEFORE UPDATE ON prompt_templates FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- ================================================================
-- §3.15 audit_logs — 审计日志（月度分区）
-- ================================================================
CREATE TABLE audit_logs (
  id          BIGINT       GENERATED ALWAYS AS IDENTITY,
  uid         BIGINT       NOT NULL DEFAULT 0,
  action      VARCHAR(64)  NOT NULL,
  resource    VARCHAR(64)  NOT NULL DEFAULT '',
  resource_id VARCHAR(64)  NOT NULL DEFAULT '',
  result      BOOLEAN      NOT NULL DEFAULT TRUE,
  ip          VARCHAR(64)  NOT NULL DEFAULT '',
  user_agent  VARCHAR(512) NOT NULL DEFAULT '',
  detail      JSONB,
  trace_id    VARCHAR(64)  NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ  NOT NULL DEFAULT now(),
  PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);

CREATE TABLE audit_logs_2026_10 PARTITION OF audit_logs
  FOR VALUES FROM ('2026-10-01') TO ('2026-11-01');

CREATE INDEX idx_audit_uid_time    ON audit_logs (uid, created_at DESC);
CREATE INDEX idx_audit_action_time ON audit_logs (action, created_at DESC);
CREATE INDEX idx_audit_trace       ON audit_logs (trace_id);


-- ================================================================
-- §8.2 knowledge_bases — 知识库表
-- ================================================================
CREATE TABLE knowledge_bases (
  kb_id            BIGINT       GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
  uid              BIGINT       NOT NULL,
  tenant           VARCHAR(64)  NOT NULL DEFAULT '',
  name             VARCHAR(128) NOT NULL,
  description      VARCHAR(512) NOT NULL DEFAULT '',
  embedding_model  VARCHAR(64)  NOT NULL DEFAULT 'qwen-text-embedding-v3',
  embedding_dim    INTEGER      NOT NULL DEFAULT 1024,
  chunk_size       INTEGER      NOT NULL DEFAULT 512,
  chunk_overlap    INTEGER      NOT NULL DEFAULT 64,
  splitter         VARCHAR(32)  NOT NULL DEFAULT 'recursive',
  doc_count        INTEGER      NOT NULL DEFAULT 0,
  chunk_count      BIGINT       NOT NULL DEFAULT 0,
  status           SMALLINT     NOT NULL DEFAULT 1,
  created_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
  updated_at       TIMESTAMPTZ  NOT NULL DEFAULT now(),
  deleted_at       TIMESTAMPTZ
);

CREATE INDEX idx_kb_uid    ON knowledge_bases (uid);
CREATE INDEX idx_kb_tenant ON knowledge_bases (tenant);
CREATE TRIGGER trg_knowledge_bases_updated_at
  BEFORE UPDATE ON knowledge_bases FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- ================================================================
-- §8.3 documents — 知识库文档表
-- ================================================================
CREATE TABLE documents (
  doc_id      BIGINT        GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
  kb_id       BIGINT        NOT NULL,
  uid         BIGINT        NOT NULL,
  title       VARCHAR(512)  NOT NULL DEFAULT '',
  file_name   VARCHAR(512)  NOT NULL DEFAULT '',
  file_type   VARCHAR(32)   NOT NULL DEFAULT 'txt',
  file_size   BIGINT        NOT NULL DEFAULT 0,
  file_hash   CHAR(64)      NOT NULL DEFAULT '',
  file_path   VARCHAR(1024) NOT NULL DEFAULT '',
  status      SMALLINT      NOT NULL DEFAULT 0,
  chunk_count INTEGER       NOT NULL DEFAULT 0,
  error_msg   TEXT,
  raw_bytes   BYTEA,
  source_url  VARCHAR(1024) NOT NULL DEFAULT '',
  metadata    JSONB,
  parsed_at   TIMESTAMPTZ,
  indexed_at  TIMESTAMPTZ,
  created_at  TIMESTAMPTZ   NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ   NOT NULL DEFAULT now(),
  deleted_at  TIMESTAMPTZ
);

CREATE INDEX idx_documents_kb        ON documents (kb_id, status);
CREATE INDEX idx_documents_uid       ON documents (uid);
CREATE INDEX idx_documents_file_hash ON documents (kb_id, file_hash);
CREATE INDEX idx_documents_metadata  ON documents USING GIN (metadata jsonb_path_ops);
CREATE TRIGGER trg_documents_updated_at
  BEFORE UPDATE ON documents FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- ================================================================
-- §8.4 chunks — 文档切片表
-- ================================================================
CREATE TABLE chunks (
  chunk_id     VARCHAR(64) PRIMARY KEY,
  doc_id       BIGINT      NOT NULL,
  kb_id        BIGINT      NOT NULL,
  seq          INTEGER     NOT NULL,
  content      TEXT        NOT NULL,
  token_count  INTEGER     NOT NULL DEFAULT 0,
  char_count   INTEGER     NOT NULL DEFAULT 0,
  page_no      INTEGER     NOT NULL DEFAULT 0,
  milvus_pk    BIGINT      NOT NULL DEFAULT 0,
  embed_model  VARCHAR(64) NOT NULL DEFAULT '',
  vec_status   SMALLINT    NOT NULL DEFAULT 0,   -- 0待入库 1已入库 2失败 3补偿中
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_chunks_doc         ON chunks (doc_id, seq);
CREATE INDEX idx_chunks_kb          ON chunks (kb_id);
CREATE INDEX idx_chunks_milvus_pk   ON chunks (milvus_pk);
CREATE INDEX idx_chunks_vec_status  ON chunks (vec_status) WHERE vec_status <> 1;
CREATE INDEX idx_chunks_content_trgm ON chunks USING GIN (content gin_trgm_ops);
CREATE TRIGGER trg_chunks_updated_at
  BEFORE UPDATE ON chunks FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- ================================================================
-- §8.5 agent_kbs — Agent 知识库挂载表
-- 全量覆盖语义（BindAgentKB 先删后插），按 kb_id 稳定输出
-- ================================================================
CREATE TABLE agent_kbs (
  agent_id   BIGINT      NOT NULL,
  kb_id      BIGINT      NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT pk_agent_kbs PRIMARY KEY (agent_id, kb_id)
);

CREATE INDEX idx_agent_kbs_kb ON agent_kbs (kb_id);


-- ================================================================
-- user_preferences — 用户偏好（JSONB 存 key-value map）
-- key-value 语义最简单，用 JSONB raw 存取；查询场景极少。
-- ================================================================
CREATE TABLE user_preferences (
  uid         BIGINT PRIMARY KEY,
  settings    JSONB  NOT NULL DEFAULT '{}'::jsonb,
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT fk_pref_user FOREIGN KEY (uid) REFERENCES users(uid) ON DELETE CASCADE
);
CREATE TRIGGER trg_user_preferences_updated_at
  BEFORE UPDATE ON user_preferences FOR EACH ROW EXECUTE FUNCTION set_updated_at();


-- ================================================================
-- §6.3 初始数据
-- ================================================================

-- 角色
INSERT INTO roles (role_id, name, description, is_system) VALUES
  (1, 'admin',   '平台管理员', TRUE),
  (2, 'user',    '普通用户',   TRUE),
  (3, 'auditor', '审计员',     TRUE)
ON CONFLICT (role_id) DO NOTHING;

-- 权限点
INSERT INTO permissions (perm_id, code, name, resource, action) VALUES
  (1,  'user:read',    '查看用户',   'user',   'read'),
  (2,  'user:write',   '编辑用户',   'user',   'write'),
  (3,  'role:assign',  '分配角色',   'role',   'assign'),
  (4,  'agent:create', '创建Agent', 'agent',  'create'),
  (5,  'agent:read',   '查看Agent', 'agent',  'read'),
  (6,  'agent:write',  '编辑Agent', 'agent',  'write'),
  (7,  'agent:delete', '删除Agent', 'agent',  'delete'),
  (8,  'llm:chat',     '调用模型',   'llm',    'chat'),
  (9,  'usage:read',   '查看用量',   'usage',  'read'),
  (10, 'audit:read',   '查看审计',   'audit',  'read'),
  (11, 'kb:create',    '创建知识库', 'kb',     'create'),
  (12, 'kb:read',      '查看知识库', 'kb',     'read'),
  (13, 'kb:write',     '编辑知识库', 'kb',     'write'),
  (14, 'kb:delete',    '删除知识库', 'kb',     'delete'),
  (15, 'kb:retrieve',  '知识库检索', 'kb',     'retrieve')
ON CONFLICT (perm_id) DO NOTHING;

-- 管理员拥有全部权限
INSERT INTO role_permissions (role_id, perm_id)
SELECT 1, perm_id FROM permissions
ON CONFLICT DO NOTHING;

-- 普通用户基础权限
INSERT INTO role_permissions (role_id, perm_id) VALUES
  (2, 4), (2, 5), (2, 6), (2, 8), (2, 9),
  (2, 11), (2, 12), (2, 13), (2, 15)
ON CONFLICT DO NOTHING;

-- 系统内置 Prompt 模板
INSERT INTO prompt_templates (template_id, uid, name, content, is_builtin) VALUES
  ('tpl_default', 0, '默认助手',
   '你是一个乐于助人的 AI 助手，请用简洁、准确的中文回答用户问题。', TRUE),
  ('tpl_summary', 0, '上下文摘要',
   '请将以下对话历史压缩为一段不超过 300 字的摘要，保留关键事实、结论与用户偏好：' || chr(10) || chr(10) || '{{history}}', TRUE),
  ('tpl_rag_qa', 0, 'RAG 问答',
   '请严格依据以下参考资料回答用户问题。若资料中确实没有相关信息，请明确说明「根据现有资料无法回答」，不要编造。'
   || chr(10) || chr(10) || '【参考资料】' || chr(10) || '{{context}}'
   || chr(10) || chr(10) || '【用户问题】' || chr(10) || '{{question}}', TRUE)
ON CONFLICT (template_id) DO NOTHING;


-- ================================================================
-- DEMO 用户（admin / alice / bob / disabled）
-- 密码统一为 Aegis@2026
-- bcrypt hash 由 pkg/passwd 生成
-- ================================================================
INSERT INTO users (uid, username, password_hash, nickname, email, status, tenant, source) VALUES
  (1001, 'admin',    '$2a$10$3hK9r2z7cbFQrW/poTUKbuXSfK8sm6YlKS1wp4JAU8Uh5t/MZk8UW', '平台管理员', 'admin@aegis.dev',   1, 'default', 'local'),
  (1002, 'alice',    '$2a$10$3hK9r2z7cbFQrW/poTUKbuXSfK8sm6YlKS1wp4JAU8Uh5t/MZk8UW', '爱丽丝',     'alice@aegis.dev',   1, 'default', 'local'),
  (1003, 'bob',      '$2a$10$3hK9r2z7cbFQrW/poTUKbuXSfK8sm6YlKS1wp4JAU8Uh5t/MZk8UW', '鲍勃',       'bob@aegis.dev',     1, 'default', 'local'),
  (1004, 'disabled', '$2a$10$3hK9r2z7cbFQrW/poTUKbuXSfK8sm6YlKS1wp4JAU8Uh5t/MZk8UW', '已禁用用户', 'disabled@aegis.dev', 2, 'default', 'local')
ON CONFLICT (uid) DO NOTHING;

-- 同步 BIGSERIAL sequence，下一个新用户 uid 将从当前 max+1 起
SELECT setval(pg_get_serial_sequence('users','uid'), COALESCE((SELECT MAX(uid) FROM users), 1));

-- 序列起点对齐内存版 ID 区间（agents≥1001 / sessions≥5001 / messages≥90001）
SELECT setval(pg_get_serial_sequence('agents','agent_id'),   1000,  true);
SELECT setval(pg_get_serial_sequence('sessions','session_id'), 5000, true);
SELECT setval(pg_get_serial_sequence('messages','msg_id'),   90000, true);

INSERT INTO user_roles (uid, role_id) VALUES
  (1001, 1), (1001, 2),  -- admin 同时拥有 admin + user
  (1002, 2),
  (1003, 2),
  (1004, 2)
ON CONFLICT DO NOTHING;

-- 完成
-- SELECT 'init.sql done' AS status;
