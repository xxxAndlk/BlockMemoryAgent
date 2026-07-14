package role

import (
	// fmt 包用于格式化错误信息，将底层错误包装成带上下文的错误返回。
	"fmt"

	// config 包提供角色配置加载能力，用于解析 roles.yaml 文件。
	"github.com/blockmemory/agent/backend/pkg/config"
)

// LoadFromFile 从指定路径加载角色配置文件（roles.yaml），
// 解析后构建并返回一个角色注册表 Registry。
//
// 参数 path：角色配置文件在文件系统中的路径。
// 返回值 *Registry：解析成功后构建的角色注册表指针；
// 返回值 error：加载或解析失败时返回包装后的错误信息。
func LoadFromFile(path string) (*Registry, error) {
	// 调用 config.LoadRoleConfig 读取并解析指定路径的角色配置。
	// 如果读取或解析失败，err 不为 nil，需要在下一步包装后返回。
	cfg, err := config.LoadRoleConfig(path)
	if err != nil {
		// 返回包装后的错误，说明失败的上下文是“加载角色配置”，
		// 这样上层调用者可以追踪到错误发生的位置和原因。
		return nil, fmt.Errorf("load role config: %w", err)
	}

	// 配置解析成功后，使用 NewRegistry 根据解析结果构建角色注册表，
	// 并将其返回给调用方。
	return NewRegistry(cfg), nil
}
