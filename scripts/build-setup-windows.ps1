#!/usr/bin/env pwsh
# Build the Windows installer (SET) from the portable single-file build.
#
#   dist/portable/ATLR-Agent.exe  ->  dist/portable/ATLR-Agent-Setup-<ver>.exe
#
# The portable exe has to exist first (scripts/build-desktop-windows.ps1); this
# script only wraps it with packaging/windows/atlr.iss, so the two deliverables
# can never drift apart. Both are published side by side in Releases.
#
# ISCC.exe is looked up in this order: -Iscc, $env:ISCC_PATH,
# .tools/InnoSetup (the local toolchain used during development), then the
# standard Inno Setup 6 install locations.
param(
	[string]$Version = '',
	[string]$Iscc = ''
)

$ErrorActionPreference = 'Continue'
$repo = Split-Path -Parent $PSScriptRoot
$portableDir = Join-Path $repo 'dist/portable'
$portableExe = Join-Path $portableDir 'ATLR-Agent.exe'

if ($Version -eq '') {
	$vLine = Select-String -Path (Join-Path $repo 'internal/version/version.go') -Pattern '^var Version = "(.*)"' | Select-Object -First 1
	if ($null -eq $vLine) { Write-Error 'cannot read Version from internal/version/version.go'; exit 1 }
	$Version = $vLine.Matches[0].Groups[1].Value
}

if (-not (Test-Path $portableExe)) {
	Write-Error "missing $portableExe — run scripts/build-desktop-windows.ps1 first"
	exit 1
}

if ($Iscc -eq '') {
	if ($env:ISCC_PATH) { $Iscc = $env:ISCC_PATH }
}
if ($Iscc -eq '') {
	$candidates = @(
		(Join-Path $repo '.tools/InnoSetup/ISCC.exe'),
		(Join-Path ${env:ProgramFiles(x86)} 'Inno Setup 6/ISCC.exe'),
		(Join-Path $env:ProgramFiles 'Inno Setup 6/ISCC.exe'),
		(Join-Path $env:LOCALAPPDATA 'Programs/Inno Setup 6/ISCC.exe')
	)
	foreach ($c in $candidates) {
		if ($c -and (Test-Path $c)) { $Iscc = $c; break }
	}
}
if (-not $Iscc -or -not (Test-Path $Iscc)) {
	Write-Error 'ISCC.exe (Inno Setup 6) not found — install it or pass -Iscc'
	exit 1
}

# Stage exactly what the installer ships: the exe plus the licence/notice the
# release is required to carry.
$stage = Join-Path $repo '.tmp/setup-stage'
New-Item -ItemType Directory -Force -Path $stage | Out-Null
Copy-Item $portableExe (Join-Path $stage 'ATLR-Agent.exe') -Force
foreach ($name in @('LICENSE.txt', 'NOTICE.md')) {
	$src = Join-Path $repo $name
	if (Test-Path $src) { Copy-Item $src (Join-Path $stage $name) -Force }
}

Write-Host "==> ISCC $Iscc"
Write-Host "==> version $Version"
& $Iscc "/DAppVersion=$Version" "/DSourceDir=$stage" "/DOutputDir=$portableDir" `
	(Join-Path $repo 'packaging/windows/atlr.iss')
if ($LASTEXITCODE -ne 0) { Write-Error "ISCC failed ($LASTEXITCODE)"; exit 1 }

$setup = Join-Path $portableDir "ATLR-Agent-Setup-$Version.exe"
if (-not (Test-Path $setup)) { Write-Error "installer missing: $setup"; exit 1 }

$hash = (Get-FileHash $setup -Algorithm SHA256).Hash.ToLower()
$shaFile = Join-Path $portableDir "ATLR-Agent-Setup-$Version.sha256"
"$hash  ATLR-Agent-Setup-$Version.exe" | Out-File -FilePath $shaFile -Encoding ascii

Write-Host "==> $setup $((Get-Item $setup).Length) bytes"
Write-Host "==> sha256 $hash"
Write-Host 'SETUP_BUILD_OK'
