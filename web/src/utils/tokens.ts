/**
 * token 数量的统一展示口径（2026-10-22）：≥1000 显示为 k，至多保留一位小数
 * （1234→1.2k，150000→150k），其余原样。所有前端 token 数字都走这里，
 * 避免同页出现 128000 与 128k 混排；也不依赖调用方的 locale。
 */
export function formatTokens(n: number | null | undefined): string {
  const v = Number(n ?? 0)
  if (!Number.isFinite(v)) return '0'
  if (Math.abs(v) < 1000) return String(Math.round(v))
  const k = v / 1000
  // ≥100k 取整（150k、128k），小数量级保留一位小数并去掉尾随 .0。
  return (Math.abs(k) >= 100 ? String(Math.round(k)) : k.toFixed(1).replace(/\.0$/, '')) + 'k'
}
