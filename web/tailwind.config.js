/** @type {import('tailwindcss').Config} */
export default {
  darkMode: 'class',
  content: [
    "./index.html",
    "./src/**/*.{vue,js,ts,jsx,tsx}",
  ],
  theme: {
    extend: {
      colors: {
        // 语义 token（跟随 CSS 变量，自动双主题）
        'primary': 'var(--bma-primary)',
        'primary-soft': 'var(--bma-primary-soft)',
        'page': 'var(--bma-page)',
        'card': 'var(--bma-card)',
        'line': 'var(--bma-border)',
        'ink': 'var(--bma-text)',
        'ink-2': 'var(--bma-text-2)',
        'ink-3': 'var(--bma-text-3)',
        // 旧色名别名：重构过渡期保持可编译，全部映射到新变量
        'dark-bg': 'var(--bma-page)',
        'dark-panel': 'var(--bma-card)',
        'dark-border': 'var(--bma-border)',
      },
      borderRadius: {
        card: '10px',
      },
      boxShadow: {
        card: 'var(--bma-card-shadow)',
      },
    },
  },
  plugins: [],
}
