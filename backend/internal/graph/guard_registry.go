package graph

// GuardRegistry 集中管理 WriteGuard / CommandGuard，把工具执行前的业务策略校验
// 从 ToolExecutor 的具体方法中解耦出来。
//
// 设计意图：
//   - 业务规则（禁止写 mailbox、禁止长运行服务器等）以独立 Guard 形式存在，便于
//     单独测试、替换和扩展。
//   - ToolExecutor 只负责调度与结果封装，不再硬编码策略。
type GuardRegistry struct {
	writeGuards   []WriteGuard
	commandGuards []CommandGuard
}

// WriteGuard 写操作策略守卫。
// 实现者按业务规则校验 path/content，命中时返回描述性错误。
type WriteGuard interface {
	Name() string
	Check(path, content string) error
}

// CommandGuard 命令执行策略守卫。
// 实现者判断命令是否被拦截，命中时返回 true 与描述性原因。
type CommandGuard interface {
	Name() string
	Check(cmd string) (blocked bool, reason string)
}

// NewGuardRegistry 创建一个空的 GuardRegistry。
func NewGuardRegistry() *GuardRegistry {
	return &GuardRegistry{}
}

// RegisterWriteGuard 注册写操作守卫。
func (g *GuardRegistry) RegisterWriteGuard(wg WriteGuard) {
	if wg == nil {
		return
	}
	g.writeGuards = append(g.writeGuards, wg)
}

// RegisterCommandGuard 注册命令执行守卫。
func (g *GuardRegistry) RegisterCommandGuard(cg CommandGuard) {
	if cg == nil {
		return
	}
	g.commandGuards = append(g.commandGuards, cg)
}

// CheckWrite 依次执行所有 WriteGuard。
// allowSpaces=true 时跳过 path-whitespace 守卫，保留与原有 allow_spaces 参数相同的语义。
func (g *GuardRegistry) CheckWrite(path, content string, allowSpaces bool) error {
	for _, wg := range g.writeGuards {
		if allowSpaces && wg.Name() == "path-whitespace" {
			continue
		}
		if err := wg.Check(path, content); err != nil {
			return err
		}
	}
	return nil
}

// CheckCommand 依次执行所有 CommandGuard，任一命中即返回。
func (g *GuardRegistry) CheckCommand(cmd string) (blocked bool, reason string) {
	for _, cg := range g.commandGuards {
		if blocked, reason := cg.Check(cmd); blocked {
			return true, reason
		}
	}
	return false, ""
}
