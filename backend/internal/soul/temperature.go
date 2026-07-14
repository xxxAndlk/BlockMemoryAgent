package soul

// TemperaturePolicy 根据任务类别与角色基准温度返回推荐温度。
//
// 实现可以是固定映射表、动态衰减、或按角色配置覆盖等不同策略。
type TemperaturePolicy interface {
	// Temperature 返回推荐温度。
	//
	// 参数：
	//   - kind：任务类别
	//   - base：角色配置文件中的默认温度，作为未命中映射时的回退
	//
	// 返回：0~1 之间的温度值。
	Temperature(kind TaskKind, base float64) float64
}

// DefaultTemperaturePolicy 使用固定映射表返回各类别推荐温度；
// 未命中映射时回退到调用方传入的 base，保证 KindGeneric/default 行为不变。
type DefaultTemperaturePolicy struct {
	table map[TaskKind]float64 // 任务类别 → 推荐温度的映射表
}

// NewDefaultTemperaturePolicy 创建包含当前默认温度表的策略。
func NewDefaultTemperaturePolicy() *DefaultTemperaturePolicy {
	// 初始化映射表，覆盖除 KindGeneric 外的所有预定义类别。
	return &DefaultTemperaturePolicy{
		table: map[TaskKind]float64{
			KindRouting:   0.0,
			KindSummarize: 0.0,
			KindCode:      0.15,
			KindAnalysis:  0.3,
			KindCreative:  0.8,
		},
	}
}

// Temperature 实现 TemperaturePolicy。
//
// 当 kind 不在 table 中（含 KindGeneric 与未知类别）时，返回 base 而非 0。
func (d *DefaultTemperaturePolicy) Temperature(kind TaskKind, base float64) float64 {
	// 在映射表中查找推荐值；命中则返回固定值。
	if v, ok := d.table[kind]; ok {
		return v
	}
	// 未命中（含 KindGeneric）回退到 base，保证调用方配置生效。
	return base
}
