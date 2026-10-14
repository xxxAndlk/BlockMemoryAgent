// sheetConvert.ts xlsx/xls → CSV 转写（2026-10-14 对话输入文件附件）。
//
// 背景：后端没有任何 Excel 解析能力（ReadFile 对二进制只会读出乱码，也没有 excelize
// 之类依赖）；前端已有 SheetJS（FileViewerSheet 同款动态加载）。因此粘贴 Excel 时
// 把每个 sheet 转成一份 CSV，随原文件一并上传，Agent 直接 ReadFile 读 CSV。
//
// 上限（解析在主线程跑，超限不转换只传原件并在 UI 提示）：
//   - 源文件 ≤ 20MiB 才尝试转换；
//   - 产物 CSV 总量 ≤ 30MiB，超出放弃全部产物。

export interface SheetConvertResult {
  /** 派生 CSV 文件（单 sheet = <stem>.csv；多 sheet = <stem>.<sheet名>.csv） */
  csvs: File[]
  /** 跳过转换的原因（未跳过为空串；UI 直接展示） */
  skipped: string
}

const CONVERT_MAX_IN_BYTES = 20 << 20
const CONVERT_MAX_OUT_BYTES = 30 << 20

/** sheet 名的文件名消毒：Windows 非法字符 → _，去首尾空白与点，空名回落 Sheet1。 */
function sanitizeSheetName(name: string): string {
  const clean = name
    .replace(/[\\/:*?"<>|]/g, '_')
    .replace(/^[\s.]+|[\s.]+$/g, '')
  return clean || 'Sheet1'
}

/** 把 xlsx/xls 文件转为 CSV 列表；任何失败都走 skipped（不抛错，调用方只传原件）。 */
export async function convertXlsxToCsv(f: File): Promise<SheetConvertResult> {
  if (f.size > CONVERT_MAX_IN_BYTES) {
    return { csvs: [], skipped: `文件较大（${(f.size / 1024 / 1024).toFixed(1)} MB），未转 CSV，已保留原文件` }
  }
  try {
    const XLSX = await import('xlsx')
    const wb = XLSX.read(new Uint8Array(await f.arrayBuffer()), { type: 'array' })
    const stem = f.name.replace(/\.[^.]+$/, '') || 'sheet'
    const multi = wb.SheetNames.length > 1
    const csvs: File[] = []
    let total = 0
    for (const name of wb.SheetNames) {
      const ws = wb.Sheets[name]
      if (!ws) continue
      const outName = multi ? `${stem}.${sanitizeSheetName(name)}.csv` : `${stem}.csv`
      const file = new File([XLSX.utils.sheet_to_csv(ws)], outName, { type: 'text/csv' })
      total += file.size
      csvs.push(file)
    }
    if (total > CONVERT_MAX_OUT_BYTES) {
      return { csvs: [], skipped: '表格转出的 CSV 体量过大，未随传，已保留原文件' }
    }
    return { csvs, skipped: '' }
  } catch (e) {
    return { csvs: [], skipped: `Excel 解析失败（${e instanceof Error ? e.message : String(e)}），已保留原文件` }
  }
}
