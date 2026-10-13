-- ================================================================
-- 为已部署的库补 DEFAULT 分区（修复"到日期必然写入失败"）
-- ================================================================
--
-- 背景：init.sql 早期版本对 messages / token_usage / audit_logs 只按
-- 月预建了 2026 Q4 的分区（audit_logs 甚至只有 2026-10），
-- 且没有 DEFAULT 分区。一旦写入的行 created_at 超出最后一个分区，
-- PostgreSQL 直接报错：
--
--   ERROR: no partition of relation "messages" found for row
--
-- messages 在对话主写入路径上，而编排层容忍写入失败
-- （见 agent_service/internal/orchestrate 的 TestChatReturnsAnswerEvenIfPersistFails），
-- 所以症状不是接口报错，而是**历史静默丢失**：每轮取不到上下文，
-- agent 看起来"失忆"。audit_logs 早在 2026-11 就会断。
--
-- 用法（对已跑过旧 init.sql 的库）：
--   docker compose exec -T postgres psql -U aegis -d aegis \
--     < configs/postgres/migrate_default_partitions.sql
--
-- 新建库不需要它：init.sql 已包含 DEFAULT 分区。
--
-- ★ 为什么写成 DO 块而不是直接 CREATE TABLE IF NOT EXISTS：
--   IF NOT EXISTS 只按**表名**判重，而 PG 的约束是"一个父表只能有一个
--   DEFAULT 分区"。若默认分区已存在但名字不同，按名字判重的写法仍会
--   执行创建并报错，导致迁移脚本在部分环境不可重跑。
--   这里直接检查父表是否已有 DEFAULT 子分区，保证幂等。

\set ON_ERROR_STOP on

DO $$
DECLARE
    t        text;
    partname text;
BEGIN
    FOREACH t IN ARRAY ARRAY['messages', 'token_usage', 'audit_logs'] LOOP
        IF NOT EXISTS (SELECT 1 FROM pg_class WHERE relname = t AND relkind = 'p') THEN
            RAISE NOTICE '表 % 不存在，跳过（可能尚未初始化）', t;
            CONTINUE;
        END IF;

        -- 已有 DEFAULT 分区就什么都不做（名字不重要，存在即合规）
        PERFORM 1
          FROM pg_inherits i
          JOIN pg_class c ON c.oid = i.inhrelid
         WHERE i.inhparent = t::regclass
           AND pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT';

        IF FOUND THEN
            RAISE NOTICE '表 % 已有 DEFAULT 分区，跳过', t;
            CONTINUE;
        END IF;

        partname := t || '_default';
        EXECUTE format('CREATE TABLE %I PARTITION OF %I DEFAULT', partname, t);
        RAISE NOTICE '已为 % 创建 DEFAULT 分区 %', t, partname;
    END LOOP;
END
$$;

-- 校验：三张表都应有且仅有一个默认分区
SELECT p.relname AS parent,
       count(*) FILTER (WHERE pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT') AS default_parts
  FROM pg_inherits i
  JOIN pg_class p ON p.oid = i.inhparent
  JOIN pg_class c ON c.oid = i.inhrelid
 WHERE p.relname IN ('messages', 'token_usage', 'audit_logs')
 GROUP BY p.relname
 ORDER BY p.relname;
