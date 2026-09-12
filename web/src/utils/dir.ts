/**
 * 目录路径规范化——仅用于"比较/去重/聚合归属"，不要用于展示或请求参数。
 *
 * 背景：会话的工作目录现在可以随时修改、且多个会话可共用同一目录，页面里到处
 * 按裸字符串比较路径（`C:\a` vs `c:\a\` vs `C:/a`）会得出"不同目录"，把同一个
 * 目录拆成多张卡片、让 `?work_dir=` 过滤查不到数据。
 *
 * 规则：统一分隔符为 `/`、去掉尾部斜杠、Windows 盘符小写；空串原样返回。
 */
export function normDir(p: string | undefined | null): string {
  if (!p) return ''
  let s = p.trim().replace(/\\/g, '/').replace(/\/+$/, '')
  if (/^[a-zA-Z]:/.test(s)) s = s[0].toLowerCase() + s.slice(1)
  return s
}

/** 两个目录是否指向同一位置（规范化比较）。 */
export function sameDir(a: string | undefined | null, b: string | undefined | null): boolean {
  return normDir(a) === normDir(b)
}
