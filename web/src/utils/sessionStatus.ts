export type StatusTagType = 'primary' | 'success' | 'danger' | 'info' | 'warning'

export function statusDotClass(status: string): string {
  switch (status) {
    case 'running':
      return 'bg-blue-400 animate-pulse'
    case 'completed':
      return 'bg-green-500'
    case 'error':
      return 'bg-red-500'
    case 'awaiting_clarify':
      return 'bg-yellow-400 animate-pulse'
    default:
      return 'bg-gray-500'
  }
}

export function statusText(status: string): string {
  const map: Record<string, string> = {
    running: '运行中',
    completed: '完成',
    error: '失败',
    awaiting_clarify: '待澄清',
  }
  return map[status] || status
}

export function statusTagType(status: string): StatusTagType {
  switch (status) {
    case 'running':
      return 'primary'
    case 'completed':
      return 'success'
    case 'error':
      return 'danger'
    default:
      return 'info'
  }
}

export function healthDotClass(service?: { online?: boolean }): string {
  return service?.online ? 'text-green-500' : 'text-red-500'
}

export function healthStatusText(service?: { online?: boolean; detail?: string }): string {
  return service?.online ? service.detail || 'Connected' : service?.detail || 'Offline'
}
