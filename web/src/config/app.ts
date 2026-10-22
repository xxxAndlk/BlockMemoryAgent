export const APP_CONFIG = {
  apiBase: '/api',
  apiTimeout: 10000,
  // SSE 断线重连（T20）：页面可见期间持续重试，指数退避封顶 30s（不再设尝试上限）。
  sseRetryMaxDelayMs: 30000,
  healthPollInterval: 10000,
  panelRefreshInterval: 3000,
  // 上下文阈值不再在这里写死：分母来自后端 GET /sessions/:id/metrics 的
  // context_token_budget（config.agent.token_budget_per_role / context_token_budget 同源）。
  warningThreshold: 0.8,
  errorThreshold: 0.95,
} as const
