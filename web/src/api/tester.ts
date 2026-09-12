import { fetchJson } from './client'

/**
 * 测试助手（验收）的工作目录级配置，与后端 GET/PUT /api/project/tester-config 对齐。
 * mode：off=任何情况不执行；auto=按 auto_prompt 由模型判断命中才执行；on=每次完成后都验收。
 * max_rounds：最大复验轮次（1-5，默认 2）。
 */
export interface TesterConfig {
  mode: 'off' | 'auto' | 'on'
  auto_prompt: string
  max_rounds: number
}

export function getTesterConfig(workDir?: string): Promise<TesterConfig> {
  // 可选 workDir：按会话目录解析（不传=server 进程目录），与 project preferences 同约定
  const q = workDir ? `?work_dir=${encodeURIComponent(workDir)}` : ''
  return fetchJson(`/project/tester-config${q}`)
}

export function saveTesterConfig(config: TesterConfig, workDir?: string): Promise<{ ok: boolean }> {
  return fetchJson('/project/tester-config', {
    method: 'PUT',
    body: JSON.stringify({ ...config, work_dir: workDir || undefined }),
  })
}
