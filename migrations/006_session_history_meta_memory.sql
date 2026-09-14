-- P0-1: 为 session_history 增加 meta_memory 列，用于持久化 MetaAgent 调度记忆。
-- 默认空数组，保证已有数据兼容。
ALTER TABLE session_history
    ADD COLUMN IF NOT EXISTS meta_memory JSONB DEFAULT '[]';
