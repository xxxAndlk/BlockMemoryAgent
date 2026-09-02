-- 008: session_history 增加每会话工作目录(S2 Web 每会话工作目录)
-- 默认空串表示回落进程默认工作目录,已有数据兼容。
ALTER TABLE session_history ADD COLUMN IF NOT EXISTS work_dir TEXT NOT NULL DEFAULT '';
