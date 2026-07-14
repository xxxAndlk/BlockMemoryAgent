package tool

// GuardRegistry 管理 WriteGuard（写入守卫）和 CommandGuard（命令守卫）两类策略检查器。
// 它维护两个切片，分别保存所有已注册的写入守卫与命令守卫，
// 在真正执行写入操作或 shell 命令前，统一调用这些守卫进行策略校验。
type GuardRegistry struct {
	// writeGuards 保存所有已注册的写入守卫，按注册顺序执行。
	writeGuards []WriteGuard
	// commandGuards 保存所有已注册的命令守卫，按注册顺序执行。
	commandGuards []CommandGuard
}

// WriteGuard 定义写入守卫接口，用于在实际写入文件之前对写入目标与内容进行校验。
type WriteGuard interface {
	// Name 返回守卫的唯一名称，便于识别、日志输出以及按名跳过特定守卫。
	Name() string
	// Check 对 path（目标路径）和 content（写入内容）进行校验；
	// 如果返回非 nil error，则表示该写入操作被阻止。
	Check(path, content string) error
}

// CommandGuard 定义命令守卫接口，用于在实际执行 shell 命令之前对命令字符串进行校验。
type CommandGuard interface {
	// Name 返回守卫的唯一名称，便于识别与日志输出。
	Name() string
	// Check 对 cmd（命令字符串）进行校验；
	// 如果 blocked 为 true，表示该命令被阻止，reason 给出阻止原因。
	Check(cmd string) (blocked bool, reason string)
}

// NewGuardRegistry 创建一个空的 GuardRegistry 实例。
// 返回的实例中 writeGuards 与 commandGuards 均为空切片。
func NewGuardRegistry() *GuardRegistry {
	return &GuardRegistry{}
}

// RegisterWriteGuard 注册一个写入守卫。
// 参数 wg 为要注册的 WriteGuard 实现；如果 wg 为 nil 则直接返回，避免空指针问题。
// 注册后，该守卫会在后续 CheckWrite 调用时按注册顺序参与校验。
func (g *GuardRegistry) RegisterWriteGuard(wg WriteGuard) {
	// 如果传入的守卫为 nil，则不进行任何操作，保证注册列表的健壮性。
	if wg == nil {
		return
	}
	// 将守卫追加到 writeGuards 切片末尾，保持注册顺序。
	g.writeGuards = append(g.writeGuards, wg)
}

// RegisterCommandGuard 注册一个命令守卫。
// 参数 cg 为要注册的 CommandGuard 实现；如果 cg 为 nil 则直接返回，避免空指针问题。
// 注册后，该守卫会在后续 CheckCommand 调用时按注册顺序参与校验。
func (g *GuardRegistry) RegisterCommandGuard(cg CommandGuard) {
	// 如果传入的守卫为 nil，则不进行任何操作，保证注册列表的健壮性。
	if cg == nil {
		return
	}
	// 将守卫追加到 commandGuards 切片末尾，保持注册顺序。
	g.commandGuards = append(g.commandGuards, cg)
}

// CheckWrite 执行所有已注册的写入守卫，对目标路径 path 和写入内容 content 进行校验。
// 参数 allowSpaces 为 true 时，会跳过名为 "path-whitespace" 的守卫（允许路径中包含空格）。
// 返回第一个返回错误的守卫的错误；若全部通过则返回 nil。
func (g *GuardRegistry) CheckWrite(path, content string, allowSpaces bool) error {
	// 遍历 writeGuards 切片中的每一个写入守卫，按注册顺序执行。
	for _, wg := range g.writeGuards {
		// 如果调用方明确允许路径中存在空格，并且当前守卫是 "path-whitespace" 守卫，
		// 则跳过该守卫，不对空格进行拦截。
		if allowSpaces && wg.Name() == "path-whitespace" {
			continue
		}
		// 调用当前守卫的 Check 方法执行具体校验。
		if err := wg.Check(path, content); err != nil {
			// 一旦有守卫返回错误，立即返回该错误，阻止后续写入操作。
			return err
		}
	}
	// 所有守卫均校验通过，返回 nil 表示允许写入。
	return nil
}

// CheckCommand 执行所有已注册的命令守卫，对命令字符串 cmd 进行校验。
// 只要任意一个守卫返回 blocked 为 true，就立即返回 true 及其阻止原因；
// 若全部通过则返回 false 和空字符串。
func (g *GuardRegistry) CheckCommand(cmd string) (blocked bool, reason string) {
	// 遍历 commandGuards 切片中的每一个命令守卫，按注册顺序执行。
	for _, cg := range g.commandGuards {
		// 调用当前守卫的 Check 方法对命令进行校验。
		if blocked, reason := cg.Check(cmd); blocked {
			// 当前守卫认为命令应被阻止，立即返回 true 与对应原因，不再检查后续守卫。
			return true, reason
		}
	}
	// 所有守卫均未阻止，返回 false 和空字符串表示允许执行该命令。
	return false, ""
}
