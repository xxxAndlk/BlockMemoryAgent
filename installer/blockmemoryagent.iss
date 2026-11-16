; BlockMemoryAgent 桌面版安装脚本（Inno Setup 6）
;
; 商业桌面应用的常规形态：per-user 安装（无需管理员）、自带运行时（PostgreSQL+pgvector、
; Redis、Node、媒体桥、Chromium）、开始菜单与桌面快捷方式、注册卸载项、缺 WebView2 时自动补装。
;
; 源目录由 bma-app.ps1 pack 生成：dist\setup\current\（已按最终安装布局铺好）。
; 编译： pwsh -File .\installer\build-installer.ps1
;
; 升级语义（2026-11-16）：
;   * 程序与依赖编排随版本覆盖；用户数据（.env / config / workspace / logs）只补缺不覆盖。
;   * 自带运行时（300MB+）按"载荷指纹"判定：没变就跳过重铺（程序升级不必重拷运行时）。
;   * 覆盖文件前先停掉旧实例（后端 + 内置 PostgreSQL/Redis + 媒体服务），否则被占用的
;     exe 拷不进去（Access denied），安装半途失败。
;   * 版本指针保留上一版本（previous），可回滚；旧版本目录只留当前 + 上一版。

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
; ① 版本标记：新版本号（纯文本）。[Code] 段据此写版本指针——
;    不用预处理器常量（/D 传的是裸值，在 [Code] 里当字符串用要处理引号）。
Source: "..\dist\setup\current\app\version.txt"; DestDir: "{app}\app"; Flags: ignoreversion
; ② 新载荷指纹：先落到临时标记，供 ④ 的 Check 与已装运行时比对
;    （程序升级但运行时没变 → 不重铺 600MB）。
Source: "..\dist\setup\current\runtime\.payload-id"; DestDir: "{app}\app"; DestName: ".new-payload-id"; Flags: ignoreversion
; ③ 程序 + 依赖编排（runtime 与版本指针单独处理，见下）
Source: "..\dist\setup\current\*"; DestDir: "{app}"; Flags: ignoreversion recursesubdirs createallsubdirs; Excludes: "runtime, runtime\*, app\current.json, app\version.txt"
; ④ 自带运行时（PostgreSQL+pgvector / Redis / Node / 媒体桥 / Chromium）：600MB+，
;    载荷指纹一致时跳过（程序升级不该重拷一遍运行时）。
Source: "..\dist\setup\current\runtime\*"; DestDir: "{app}\runtime"; Flags: ignoreversion recursesubdirs createallsubdirs; Check: NeedsRuntime
; 用户数据种子：只在缺失时铺（升级绝不覆盖用户的 .env / config）
Source: "..\dist\setup\current\config-templates\*"; DestDir: "{app}\config"; Flags: onlyifdoesntexist recursesubdirs
; plugins.yaml 是"程序侧模板 + 用户可改"的混合体：桌面版随包把容器类插件改写成
; 本地 stdio，升级时必须能更新它——但**只在用户没改过时**（与上次随包那份逐字节一致）。
; 顺序要紧：先用旧 .packaged 比对，再刷新 .packaged。
Source: "..\dist\setup\current\config-templates\plugins.yaml"; DestDir: "{app}\config"; Flags: ignoreversion; Check: PluginsUnchanged
Source: "..\dist\setup\current\config-templates\plugins.yaml"; DestDir: "{app}\config"; DestName: "plugins.yaml.packaged"; Flags: ignoreversion
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

[UninstallDelete]
; 版本指针是安装器手写的（不在 [Files] 里，卸载器不认识它）→ 卸载时一并清掉。
Type: files; Name: "{app}\app\current.json"

[Code]
var
  OldVersion: String;
  NewVersion: String;

function WebView2RuntimePresent: Boolean;
var
  Version: string;
begin
  Result :=
    RegQueryStringValue(HKLM, 'SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}', 'pv', Version) or
    RegQueryStringValue(HKLM, 'SOFTWARE\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}', 'pv', Version);
end;

// ReadTextFile 读文本文件内容（ANSI 读入；UTF-8 载荷指纹是纯 ASCII，无碍）。
function ReadTextFile(const FileName: String): String;
var
  S: AnsiString;
begin
  Result := '';
  if LoadStringFromFile(FileName, S) then
    Result := Trim(String(S));
end;

// NeedsRuntime 判定是否要铺自带运行时：载荷指纹一致 → 运行时没变 → 跳过。
// 指纹由 bma-app.ps1 pack 写入（配方号 + 载荷文件清单摘要）；新指纹先由 [Files]
// 落到 {app}\app\.new-payload-id（安装器读不到源目录路径，只能这么绕）。
function NeedsRuntime: Boolean;
var
  InstalledId, NewId: String;
begin
  Result := True;
  InstalledId := ReadTextFile(ExpandConstant('{app}\runtime\.payload-id'));
  NewId := ReadTextFile(ExpandConstant('{app}\app\.new-payload-id'));
  if (NewId <> '') and (InstalledId = NewId) then
    Result := False;
end;

// PluginsUnchanged 判定用户是否改过 config\plugins.yaml：
// 与上次随包的那份（config\plugins.yaml.packaged）逐字节一致 = 没改过 → 可随版本更新。
function PluginsUnchanged: Boolean;
var
  Cur, Packaged: AnsiString;
begin
  Result := False;
  if not LoadStringFromFile(ExpandConstant('{app}\config\plugins.yaml'), Cur) then
    Exit;                                   // 还没有 → 由 onlyifdoesntexist 那条铺
  if not LoadStringFromFile(ExpandConstant('{app}\config\plugins.yaml.packaged'), Packaged) then
    Exit;                                   // 没有参照物 → 不动用户的文件
  Result := (Cur = Packaged);
end;

// ExtractJsonString 从 JSON 文本里取字符串字段的值（本项目指针文件格式固定，
// 不引入完整 JSON 解析；只认第一个 "key" 后的第一个字符串值）。
function ExtractJsonString(const Content: String; const Key: String): String;
var
  P: Integer;
  S: String;
begin
  Result := '';
  P := Pos('"' + Key + '"', Content);
  if P = 0 then Exit;
  S := Copy(Content, P + Length(Key) + 2, 4096);
  P := Pos(':', S);
  if P = 0 then Exit;
  Delete(S, 1, P);
  P := Pos('"', S);
  if P = 0 then Exit;
  Delete(S, 1, P);
  P := Pos('"', S);
  if P = 0 then Exit;
  Result := Copy(S, 1, P - 1);
end;

// WriteCurrentJson 写版本指针：previous 保留上一版本（供 bma-app.ps1 rollback）。
// 安装失败时不会走到这里——指针保持旧值，回滚语义不破。
// 注意：不写 installed_at —— Inno 的 Pascal Script 没有日期格式化函数
// （FormatDateTime/YearOf 均不可用），时间戳留给 bma-app.ps1 那条安装路径。
// 内容是纯 ASCII，SaveStringToFile（ANSI 写）足够。
procedure WriteCurrentJson;
var
  Json: String;
begin
  Json := '{' + #13#10 +
    '  "version": "' + NewVersion + '",' + #13#10 +
    '  "previous": "' + OldVersion + '",' + #13#10 +
    '  "package": "installer"' + #13#10 +
    '}';
  SaveStringToFile(ExpandConstant('{app}\app\current.json'), Json, False);
end;

// PruneOldVersions 清理旧版本目录：只留当前版本与上一版本。
procedure PruneOldVersions;
var
  FindRec: TFindRec;
  Dir, Entry: String;
begin
  Dir := ExpandConstant('{app}\app\versions');
  if not DirExists(Dir) then Exit;
  if FindFirst(Dir + '\*', FindRec) then
  begin
    try
      repeat
        if (FindRec.Attributes and FILE_ATTRIBUTE_DIRECTORY) <> 0 then
        begin
          Entry := FindRec.Name;
          // 必须排除 "." 与 ".."：FindFirst 会返回它们，
          // DelTree('...\versions\..') 会把上一级（{app}\app）整个删掉。
          if (Entry <> '.') and (Entry <> '..') and
             (Entry <> NewVersion) and ((OldVersion = '') or (Entry <> OldVersion)) then
            DelTree(Dir + '\' + Entry, True, True, True);
        end;
      until not FindNext(FindRec);
    finally
      FindClose(FindRec);
    end;
  end;
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  ResultCode: Integer;
  Content: AnsiString;
begin
  if CurStep = ssInstall then
  begin
    // 覆盖文件前先读旧版本指针（后面写 previous 用）。
    if LoadStringFromFile(ExpandConstant('{app}\app\current.json'), Content) then
      OldVersion := ExtractJsonString(String(Content), 'version');

    // 停掉旧实例：后端 + 内置 PostgreSQL/Redis + 媒体服务都占着 {app} 下的 exe，
    // 不停掉覆盖文件会 Access denied（安装半途失败）。
    if FileExists(ExpandConstant('{app}\{#MyAppExeName}')) then
      Exec(ExpandConstant('{app}\{#MyAppExeName}'), '--stop', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
  end;
  if CurStep = ssPostInstall then
  begin
    // 新版本号来自载荷里的 version.txt（[Files] 已把它铺到 {app}\app）。
    NewVersion := ReadTextFile(ExpandConstant('{app}\app\version.txt'));
    if OldVersion = NewVersion then
      OldVersion := '';
    WriteCurrentJson;
    PruneOldVersions;
    // 清掉比对用的临时指纹标记
    DeleteFile(ExpandConstant('{app}\app\.new-payload-id'));
  end;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
begin
  if CurUninstallStep = usPostUninstall then
  begin
    // 用户数据（%LOCALAPPDATA%\BlockMemoryAgent：数据库数据、日志、工作目录）默认保留，
    // 避免误删会话与记忆；需要彻底清理时由用户自行删除该目录。
  end;
end;
