import { ref } from 'vue'

const KEY = 'bma:theme'
type Theme = 'light' | 'dark'

const theme = ref<Theme>(localStorage.getItem(KEY) === 'dark' ? 'dark' : 'light')

function apply(t: Theme) {
  document.documentElement.classList.toggle('dark', t === 'dark')
}

// 模块加载即应用，保证首屏不出现错误主题闪烁。
apply(theme.value)

export function useTheme() {
  const toggleTheme = () => {
    theme.value = theme.value === 'dark' ? 'light' : 'dark'
    localStorage.setItem(KEY, theme.value)
    apply(theme.value)
  }
  return { theme, toggleTheme }
}
