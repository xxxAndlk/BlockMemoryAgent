; BlockMemoryAgent 桌面版安装脚本（Inno Setup 6）
;
; 商业桌面应用的常规形态：per-user 安装（无需管理员）、自带运行时（PostgreSQL+pgvector、
; Redis）、开始菜单与桌面快捷方式、注册卸载项、缺 WebView2 时自动补装。
;
; 源目录由 bma-app.ps1 pack 生成：dist\setup\current\（已按最终安装布局铺好）。
; 编译： pwsh -File .\installer\build-installer.ps1

#define MyAppName "BlockMemoryAgent"
#ifndef MyAppVersion
  #define MyAppVersion "0.0.0-dev"
#endif
#define MyAppPublisher "BlockMemoryAgent"
#define MyAppExeName "BMA-Desktop.exe"
#define MyAppId "{{7C3D6A11-9E42-4B7C-8F55-2D0B6E4A9C31}"

[Setup]
AppId={#MyAppId}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppVerName={#MyAppName} {#MyAppVersion}
AppPublisher={#MyAppPublisher}
DefaultDirName={localappdata}\Programs\BlockMemoryAgent
DefaultGroupName={#MyAppName}
DisableProgramGroupPage=yes
; per-user：装到 %LOCALAPPDATA%，不需要管理员权限（运行时数据也在其下）
PrivilegesRequired=lowest
PrivilegesRequiredOverridesAllowed=dialog
OutputDir=..\dist\installer
OutputBaseFilename=BlockMemoryAgent-Setup-{#MyAppVersion}
SetupIconFile=..\assets\icon.ico
UninstallDisplayIcon={app}\{#MyAppExeName}
UninstallDisplayName={#MyAppName}
Compression=lzma2/ultra64
SolidCompression=yes
WizardStyle=modern
; 升级时若应用在运行，用 Restart Manager 提示关闭（否则文件被锁，升级会半途失败）
CloseApplications=yes
; 关掉"选择安装位置"页之外的额外步骤，保持安装过程安静
ShowLanguageDialog=auto
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible

[Languages]
Name: "chinese"; MessagesFile: "compiler:Default.isl"

[Files]
; 程序 + 自带运行时 + 依赖编排：整目录铺（源已按最终布局组织）
Source: "..\dist\setup\current\*"; DestDir: "{app}"; Flags: ignoreversion recursesubdirs createallsubdirs
; 用户数据种子：只在缺失时铺（升级绝不覆盖用户的 .env / config）
Source: "..\dist\setup\current\config-templates\*"; DestDir: "{app}\config"; Flags: onlyifdoesntexist recursesubdirs
Source: "..\dist\setup\current\.env.example"; DestDir: "{app}"; DestName: ".env"; Flags: onlyifdoesntexist
; WebView2 引导安装程序（目标机缺运行时时才跑）
Source: "webview2\MicrosoftEdgeWebview2Setup.exe"; DestDir: "{app}\installer\webview2"; Flags: skipifsourcedoesntexist ignoreversion

[Icons]
Name: "{group}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"
Name: "{group}\卸载 {#MyAppName}"; Filename: "{uninstallexe}"
; 桌面快捷方式无条件创建：静默安装（/VERYSILENT）不弹任务页，挂在 Tasks 上会被跳过，
; 用户升级后桌面就找不到入口了。
Name: "{autodesktop}\{#MyAppName}"; Filename: "{app}\{#MyAppExeName}"

[Run]
; 目标机没有 WebView2 运行时则静默补装（Windows 11 预装 Edge 时通常已存在）
Filename: "{app}\installer\webview2\MicrosoftEdgeWebview2Setup.exe"; Parameters: "/silent /install"; Flags: runhidden waituntilterminated; Check: not WebView2RuntimePresent
Filename: "{app}\{#MyAppExeName}"; Description: "立即启动 {#MyAppName}"; Flags: nowait postinstall skipifsilent

[UninstallRun]
; 卸载前先停掉后台服务与自带运行时（否则 PG 会占着数据目录）
Filename: "{app}\{#MyAppExeName}"; Parameters: "--stop"; Flags: runhidden waituntilterminated; RunOnceId: "StopApp"

[Code]
function WebView2RuntimePresent: Boolean;
var
  Version: string;
begin
  Result :=
    RegQueryStringValue(HKLM, 'SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}', 'pv', Version) or
    RegQueryStringValue(HKLM, 'SOFTWARE\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}', 'pv', Version);
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usPostUninstall then
  begin
    // 用户数据（%LOCALAPPDATA%\BlockMemoryAgent：数据库数据、日志、工作目录）默认保留，
    // 避免误删会话与记忆；需要彻底清理时由用户自行删除该目录。
  end;
end;
