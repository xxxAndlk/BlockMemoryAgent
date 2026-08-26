package tool

// contract.go 定义跨域契约（TODO #57）：WriteSpec 的 contract 字段承载派发时
// 父 Agent 显式钉死的跨域引用协议，dispatcher 在父节点下全部兄弟域完成后
// 跑静态契约检查器（regex/文本解析，零 LLM）逐条核对。
//
// 设计约束（来自 doc/TODO.md #57 不做项）：
//   - 不做 AST 级精确解析，regex 够用，漏报优于复杂化；
//   - 不强制所有 spec 填 contract（单域/无跨域引用任务可空），空契约检查器跳过；
//   - 契约只保证静态一致性，语义协调仍靠集成验证例外通道（#58）。

// Contract 是跨域引用协议的结构化载体。四类条目均可空；全空等价于无契约。
type Contract struct {
	// Symbols 跨域符号映射：Symbol 声明于 File，Refs 列引用方文件。
	// 检查器验证 Symbol 在声明文件存在、每个引用方文件含引用点。
	Symbols []ContractSymbol `json:"symbols,omitempty" yaml:"symbols,omitempty"`
	// DOMIDs DOM 集成点清单：ID 必须声明于 File（HTML/JSX）。
	DOMIDs []ContractDOMID `json:"dom_ids,omitempty" yaml:"dom_ids,omitempty"`
	// Scripts script 加载顺序：条目顺序即加载顺序，检查器在 HTML 中核对相对顺序。
	Scripts []ContractScript `json:"scripts,omitempty" yaml:"scripts,omitempty"`
	// Signatures 跨域函数签名：Signature 文本必须出现在 File 中（如 `attack(target, dmg)`）。
	Signatures []ContractSignature `json:"signatures,omitempty" yaml:"signatures,omitempty"`
}

// Empty 判断契约是否无任何条目（等价于未填）。
func (c *Contract) Empty() bool {
	if c == nil {
		return true
	}
	return len(c.Symbols) == 0 && len(c.DOMIDs) == 0 && len(c.Scripts) == 0 && len(c.Signatures) == 0
}

// ContractSymbol 跨域符号映射条目。
type ContractSymbol struct {
	// Symbol 符号名（如 `GameEngine.init`、`window.CONFIG`），正则转义后按字面匹配。
	Symbol string `json:"symbol" yaml:"symbol"`
	// File 声明该符号的文件路径（相对工作目录）。
	File string `json:"file" yaml:"file"`
	// Refs 引用该符号的文件路径列表。
	Refs []string `json:"refs,omitempty" yaml:"refs,omitempty"`
	// Stub 占位桩标记（TODO #61）：true 表示该符号当前是占位实现（骨架期建的桩），
	// 必须由 Owner 领域实装。dispatcher 在兄弟域全完成时检查声明文件不再含占位标记，
	// 仍含则判孤儿桩打回责任方。
	Stub bool `json:"stub,omitempty" yaml:"stub,omitempty"`
	// Owner 占位桩责任方（TODO #61）：负责实装该桩的领域名（与派发 domain 名对应）。
	// Stub=true 时必填；进入 Owner 的验收清单（验收时须确认已实装）。
	Owner string `json:"owner,omitempty" yaml:"owner,omitempty"`
}

// ContractDOMID DOM 集成点条目。
type ContractDOMID struct {
	// ID 元素 id（如 `game-canvas`）。
	ID string `json:"id" yaml:"id"`
	// File 声明该 id 的 HTML 文件路径。
	File string `json:"file" yaml:"file"`
}

// ContractScript script 加载顺序条目。
type ContractScript struct {
	// File script 文件路径（如 `js/engine.js`），与 HTML 中 src 的 basename 匹配。
	File string `json:"file" yaml:"file"`
}

// ContractSignature 跨域函数签名条目。
type ContractSignature struct {
	// Symbol 函数名（人读标签，检查以 Signature 文本为准）。
	Symbol string `json:"symbol" yaml:"symbol"`
	// Signature 期望的签名文本（如 `attack(target, dmg)`、`constructor(x, y)`），
	// 必须字面出现在 File 中。
	Signature string `json:"signature" yaml:"signature"`
	// File 声明该签名的文件路径。
	File string `json:"file" yaml:"file"`
}
