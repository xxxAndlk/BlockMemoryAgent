-- 009: session_history 增加 status 列 + session_id 唯一索引
-- status 记录会话最后一次落库时的状态;进程死亡遗留 running 的行在恢复时被标记为"因服务重启中断"。
-- 存量库 session_id 无唯一约束(此前 ON CONFLICT DO NOTHING 无冲突目标,每轮落一行),
-- 先按 id 保留最新一行去重,再建唯一索引,SaveHistory 的 ON CONFLICT (session_id) DO UPDATE 依赖此索引。
ALTER TABLE session_history ADD COLUMN IF NOT EXISTS status VARCHAR(32) NOT NULL DEFAULT 'completed';

-- 去重: 同 session_id 保留 id 最大的一行(最后一次写入)。
DELETE FROM session_history a USING session_history b
    WHERE a.session_id = b.session_id AND a.id < b.id;

CREATE UNIQUE INDEX IF NOT EXISTS uniq_session_history_session_id
    ON session_history (session_id);
