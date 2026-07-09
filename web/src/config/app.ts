export const APP_CONFIG = {
  apiBase: '/api',
  apiTimeout: 10000,
  sseRetry: 5,
  sseRetryMaxDelayMs: 16000,
  healthPollInterval: 10000,
  panelRefreshInterval: 3000,
  tokenContextLimit: 128000,
  warningThreshold: 0.8,
  errorThreshold: 0.95,
} as const
