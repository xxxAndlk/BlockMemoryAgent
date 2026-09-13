export function useSessionStatus() {
  function statusDotClass(status: string) {
    switch (status) {
      case 'running':
        return 'bg-blue-400 animate-pulse'
      case 'completed':
        return 'bg-green-500'
      case 'error':
        return 'bg-red-500'
      case 'awaiting_clarify':
        return 'bg-yellow-400 animate-pulse'
      case 'awaiting_child':
        // 挂起等子：蓝色系（sky 与 running 的 blue 区分），静态不闪——会话挂起而非活跃执行
        return 'bg-sky-400'
      default:
        return 'bg-gray-500'
    }
  }

  function statusText(status: string) {
    const map: Record<string, string> = {
      running: '运行中',
      completed: '完成',
      error: '失败',
      awaiting_clarify: '待澄清',
      awaiting_child: '挂起等子',
    }
    return map[status] || status
  }

  function healthDotClass(service?: { online?: boolean }) {
    return service?.online ? 'text-green-500' : 'text-red-500'
  }

  function healthStatusText(service?: { online?: boolean; detail?: string }) {
    return service?.online ? service.detail || 'Connected' : service?.detail || 'Offline'
  }

  return {
    statusDotClass,
    statusText,
    healthDotClass,
    healthStatusText,
  }
}
