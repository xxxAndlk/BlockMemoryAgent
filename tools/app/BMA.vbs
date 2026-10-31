' BlockMemoryAgent silent launcher: runs app\launch.ps1 hidden (no console flash).
' Shortcuts target this file so double-click behaves like a desktop app.
Set fso = CreateObject("Scripting.FileSystemObject")
Set sh = CreateObject("WScript.Shell")
root = fso.GetParentFolderName(WScript.ScriptFullName)
cmd = "powershell -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File """ & root & "\app\launch.ps1"""
sh.Run cmd, 0, False
