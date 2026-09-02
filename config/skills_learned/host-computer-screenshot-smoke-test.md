---
name: windows-host-smoke-test
title: Windows主机基础功能冒烟测试
when_to_use: 当需要验证主机的截图、应用启动激活、鼠标点击、键盘输入等基础工具功能时使用
outcome: success
---

## 步骤
1. 调用screenshot工具捕获当前屏幕
2. 调用list_running_apps列出当前运行进程
3. 调用open_application启动目标测试应用
4. 再次调用list_running_apps确认目标进程存在
5. 调用activate_app激活目标应用
6. 调用left_click点击目标应用的文本输入区域
7. 调用type工具输入测试文本
8. 再次调用screenshot捕获屏幕验证文本是否落屏

## 坑点
- 未区分传统Win32应用和UWP应用的进程名差异，导致启动激活失败
- 未提前验证应用进程存在就执行后续操作
- 未确认前台窗口状态就执行鼠标点击或键盘输入
- 未在输入后验证文本是否成功落屏

## 验证
通过list_running_apps确认目标应用进程存在，通过screenshot确认目标应用为前台窗口，通过再次screenshot确认测试文本已成功显示在屏幕上
