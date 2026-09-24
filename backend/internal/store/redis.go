package store

import (
	"context" // 上下文,贯穿所有 Redis 调用以支持超时与取消
	"fmt"     // 格式化错误
	"time"    // Ping 超时控制

	"github.com/redis/go-redis/v9" // Redis 客户端
)

// RedisStore 是 Redis 存储层复合入口。
// 职责: 按领域拆分为子 Store,自身保留共享的 Redis 客户端。
// 并发安全: go-redis 客户端内部维护连接池,可在多 goroutine 间共享。
type RedisStore struct {
	client *redis.Client // 共享 Redis 客户端

	AgentMsg *AgentMsgRedisStore // Agent 消息热层（编排页对话视图）
}

// NewRedisStore 创建 Redis 存储实例。
// 参数:
//   - ctx:      用于 Ping 超时控制的上下文
//   - addr:     Redis 地址 (host:port)
//   - password: 认证密码,空表示无密码
//   - db:       Redis 数据库编号
//
// 返回:
//   - *RedisStore: 已通过 Ping 校验的存储实例
//   - error: Ping 失败时返回包装错误
func NewRedisStore(ctx context.Context, addr, password string, db int) (*RedisStore, error) {
	// 初始化 go-redis 客户端，尚未真正建连
	client := redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
	// 主动 Ping 一次,提前暴露网络/凭证类错误；使用超时 context 避免启动挂死
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	// 装配子 Store，注入共享 client
	return &RedisStore{
		client:   client,
		AgentMsg: &AgentMsgRedisStore{client: client},
	}, nil
}

// Close 关闭 Redis 客户端连接。
// 调用后该 store 不可再用。
// 返回: 关闭错误。
func (s *RedisStore) Close() error {
	return s.client.Close()
}

// Ping 检查 Redis 连通性,用于健康检查。
// 参数:
//   - ctx: 超时控制
//
// 返回: Ping 错误。
func (s *RedisStore) Ping(ctx context.Context) error {
	return s.client.Ping(ctx).Err()
}
