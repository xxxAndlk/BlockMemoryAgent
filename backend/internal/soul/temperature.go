package soul

// TemperaturePolicy 根据任务类别与角色基准温度返回推荐温度。
//
// 实现可以是固定映射表、动态衰减、或按角色配置覆盖等不同策略。
type TemperaturePolicy interface {
	Temperature(kind TaskKind, base float64) float64
}

// DefaultTemperaturePolicy 使用固定映射表返回各类别推荐温度；
// 未命中映射时回退到调用方传入的 base，保证 KindGeneric/default 行为不变。
type DefaultTemperaturePolicy struct {
	table map[TaskKind]float64
}

// NewDefaultTemperaturePolicy 创建包含当前默认温度表的策略。
func NewDefaultTemperaturePolicy() *DefaultTemperaturePolicy {
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
	if v, ok := d.table[kind]; ok {
		return v
	}
	return base
}
