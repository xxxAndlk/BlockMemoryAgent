export const APP_CONFIG = {
  apiBase: '/api',
  apiTimeout: 10000,
  // SSE 断线重连（T20）：页面可见期间持续重试，指数退避封顶 30s（不再设尝试上限）。
  sseRetryMaxDelayMs: 30000,
  healthPollInterval: 10000,
  panelRefreshInterval: 3000,
  tokenContextLimit: 128000,
  warningThreshold: 0.8,
  errorThreshold: 0.95,
} as const
