package dag

import "context"

// Store DAG 持久化接口（由 store.PostgresStore 等外部存储实现）。
//
// 设计意图：将 dag 包的调度实现与持久化层解耦，使调度器可以独立测试，
// 也便于后续替换为 Redis、SQLite 等其他后端而无需改动调度逻辑。
type Store interface {
	// SaveDAG 保存或更新一条 DAG 定义。
	//
	// 参数：
	//   - ctx：上下文，用于取消与超时。
	//   - d：待持久化的 DAG 实例。
	//
	// 返回：持久化失败时返回 error。
	SaveDAG(ctx context.Context, d *DAG) error
	// GetDAG 按 ID 读取一条 DAG 定义。
	//
	// 参数：
	//   - ctx：上下文。
	//   - id：DAG 唯一标识。
	//
	// 返回：找到的 DAG；若不存在返回 nil 与 nil error；其他错误返回非 nil error。
	GetDAG(ctx context.Context, id string) (*DAG, error)
	// ListDAGs 列出所有 DAG 定义。
	//
	// 参数：
	//   - ctx：上下文。
	//
	// 返回：DAG 实例切片；失败时返回 error。
	ListDAGs(ctx context.Context) ([]*DAG, error)
	// DeleteDAG 按 ID 删除 DAG 定义。
	//
	// 参数：
	//   - ctx：上下文。
	//   - id：DAG 唯一标识。
	//
	// 返回：删除失败时返回 error。
	DeleteDAG(ctx context.Context, id string) error
}
