// Package computeruse 预留 Computer Use 能力（P3-5）。
//
// 设计意图：将浏览器自动化、GUI 操作、截图对比等能力封装为独立工具集，
// 后续通过 plugins.ToolRegistry 注册给 Agent 使用。
// 当前仅保留包占位，避免提前实现绑定死具体协议（CDP / PyAutoGUI / Accessibility Tree）。
//
// 能力范围（后续实现时参考）：
//   - 浏览器：导航、截图、点击、输入、滚动、获取元素文本（chromedp / Playwright）
//   - 桌面：截屏、鼠标/键盘操作、OCR 定位
//   - 安全：所有操作需用户显式授权，禁止自动执行高风险动作
package computeruse
