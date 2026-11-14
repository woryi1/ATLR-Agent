; ATLR-Agent — per-user Windows installer for the single-file portable app.
;
; The portable build (dist/portable/ATLR-Agent.exe) is self-contained: web UI
; and ripgrep are embedded, and it creates Data/ beside itself on first launch.
; This installer only gives it a stable home, a Start-menu entry and an
; uninstaller — no PATH edits, no bundled uv, no service.
;
; Per-user ({userpf}) on purpose: Data/ has to be writable by the app, and a
; machine-wide install would need UAC and an admin-writable data directory.
; Uninstall removes only what was installed; the app's own Data/ (user data)
; is left in place because Inno only deletes files it knows about.
;
; Compile locally (see scripts/build-setup-windows.ps1):
;   ISCC.exe /DAppVersion=0.7 /DSourceDir=<stage> /DOutputDir=<dir> atlr.iss

#ifndef AppVersion
  #define AppVersion "0.0.0-dev"
#endif
#ifndef SourceDir
  #define SourceDir "."
#endif
#ifndef OutputDir
  #define OutputDir "output"
#endif
#ifndef OutputName
  #define OutputName "ATLR-Agent-Setup"
#endif

[Setup]
; Stable AppId so a newer installer upgrades in place instead of stacking a
; second copy. Never change this value.
AppId={{9C4B1A7E-3F52-4D68-B0A9-7E2C5D14F8B3}
AppName=ATLR-Agent
AppVersion={#AppVersion}
AppVerName=ATLR-Agent {#AppVersion}
AppPublisher=woryi
AppPublisherURL=https://gitee.com/woryi2/ATLR-Agent
AppSupportURL=https://github.com/woryi1/ATLR-Agent
DefaultDirName={userpf}\ATLR-Agent
DefaultGroupName=ATLR-Agent
DisableProgramGroupPage=yes
DisableDirPage=yes
PrivilegesRequired=lowest
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
OutputDir={#OutputDir}
OutputBaseFilename={#OutputName}-{#AppVersion}
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
SetupIconFile=..\..\cmd\octo-desktop\build\windows\icon.ico
UninstallDisplayName=ATLR-Agent {#AppVersion}
UninstallDisplayIcon={app}\ATLR-Agent.exe

[Files]
Source: "{#SourceDir}\ATLR-Agent.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#SourceDir}\LICENSE.txt"; DestDir: "{app}"; Flags: ignoreversion skipifsourcedoesntexist
Source: "{#SourceDir}\NOTICE.md"; DestDir: "{app}"; Flags: ignoreversion skipifsourcedoesntexist

[Icons]
Name: "{userprograms}\ATLR-Agent"; Filename: "{app}\ATLR-Agent.exe"; \
  WorkingDir: "{app}"; IconFilename: "{app}\ATLR-Agent.exe"; Comment: "ATLR-Agent"

[Run]
Filename: "{app}\ATLR-Agent.exe"; Description: "启动 ATLR-Agent"; \
  Flags: nowait postinstall skipifsilent

[Code]
// Close a running copy so an in-place upgrade can replace the image. The app
// holds its own exe open while running; without this the copy step fails.
procedure CurStepChanged(CurStep: TSetupStep);
var
  ResultCode: Integer;
begin
  if CurStep = ssInstall then
    Exec(ExpandConstant('{cmd}'), '/C taskkill /IM ATLR-Agent.exe /F >nul 2>&1',
         '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
end;
