/** @type {import('tailwindcss').Config} */
export default {
  content: [
    "./index.html",
    "./src/**/*.{vue,js,ts,jsx,tsx}",
  ],
  theme: {
    extend: {
      colors: {
        'dark-bg': '#0f1219',
        'dark-panel': '#1a1d24',
        'dark-border': '#2a2d35',
        'primary': '#3b82f6',
      }
    },
  },
  plugins: [],
}
