package dag

import "context"

// Store DAG 持久化接口（由 store.PostgresStore 实现）。
// 从 dag.go 拆出，使 dag 包的接口定义与调度实现分离，便于独立测试与替换存储实现。
type Store interface {
	SaveDAG(ctx context.Context, d *DAG) error
	GetDAG(ctx context.Context, id string) (*DAG, error)
	ListDAGs(ctx context.Context) ([]*DAG, error)
	DeleteDAG(ctx context.Context, id string) error
}
