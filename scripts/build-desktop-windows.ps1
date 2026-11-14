#!/usr/bin/env pwsh
# Build the Windows delivery artefacts for this brand only:
#   dist/windows-amd64/<ExeName>   (the GUI exe, bundled CLI alongside)
#   dist/portable/<ExeName>        (single-file portable deliverable)
# This is the ONLY supported way to produce them; hand-copied `go build`
# invocations are how the black console window came back.
#
# There is deliberately NO zip and no installer step: the single exe sitting in
# dist/portable is the deliverable (portable by construction -- it creates
# Data/ beside itself). An archive is produced only on an explicit request, and
# never as a side effect of packaging.
#
# Why every flag below is mandatory:
#   -H windowsgui   PE subsystem 2 (GUI). Without it the exe is subsystem 3 and
#                   Windows allocates a console for a GUI app.
#   -tags embedrg   embeds the ripgrep binary; grep/glob shell out to it and
#                   error out on a clean machine without it.
#   -H and -X in ONE -ldflags string: a second -ldflags replaces the first, and
#                   the version injection then silently wins over the subsystem.
#   CGO_ENABLED=0   WebView2 bindings are pure Go; matches CI.
# The script fails if the finished exe is not subsystem 2, and if the brand
# identity it injected is not present in the binary -- a missing -X is a silent
# fallback, and the fallback shares the old build's mutex and port.
param(
	[string]$Version = '',
	[string]$Commit = 'unknown',
	[string]$RgVersion = '15.1.0'
)

# Continue, not Stop: npm/vite and go write progress and warnings to stderr,
# and PowerShell 5.1 turns any native stderr line into a NativeCommandError
# that Stop would treat as fatal in the middle of a healthy build. Every native
# step below is followed by an explicit $LASTEXITCODE check and exit 1.
$ErrorActionPreference = 'Continue'
$repo = Split-Path -Parent $PSScriptRoot
$outDir = Join-Path $repo 'dist/windows-amd64'
$portableDir = Join-Path $repo 'dist/portable'
$brandFile = Join-Path $repo 'internal/brand/brand.go'
$rgBin = Join-Path $repo 'internal/tools/rgembed/binaries/rg'

if ($Version -eq '') {
	$vLine = Select-String -Path (Join-Path $repo 'internal/version/version.go') -Pattern '^var Version = "(.*)"' | Select-Object -First 1
	if ($null -eq $vLine) { Write-Error 'cannot read Version from internal/version/version.go'; exit 1 }
	$Version = $vLine.Matches[0].Groups[1].Value
}

# The brand lives in one place (internal/brand); read it back instead of
# repeating the values here, so the exe name this script writes and the
# identity it injects cannot drift apart.
function Read-BrandVar([string]$name) {
	$line = Select-String -Path $brandFile -Pattern ('^var ' + $name + ' = "(.*)"') | Select-Object -First 1
	if ($null -eq $line) { Write-Error "cannot read brand.$name from $brandFile"; exit 1 }
	return $line.Matches[0].Groups[1].Value
}

$brandName = Read-BrandVar 'Name'
$brandAppID = Read-BrandVar 'AppID'
$brandSID = Read-BrandVar 'SingleInstanceID'
$brandPort = Read-BrandVar 'DesktopPort'
$brandUpd = Read-BrandVar 'UpstreamUpdates'
$brandExe = Read-BrandVar 'ExeName'

$desktopExe = Join-Path $outDir $brandExe
$cliExe = Join-Path $outDir 'octo.exe'
$portableExe = Join-Path $portableDir $brandExe

if ((Split-Path -Leaf $desktopExe) -ne $brandExe) {
	Write-Error "exe name $((Split-Path -Leaf $desktopExe)) does not match internal/brand.ExeName=$brandExe"
	exit 1
}

Write-Host "==> brand $brandName ($brandAppID) port $brandPort upstream-updates $brandUpd"
Write-Host "==> version $Version ($Commit)"

New-Item -ItemType Directory -Force -Path $outDir | Out-Null

Write-Host '==> [1/6] npm run build'
Push-Location (Join-Path $repo 'web')
# vite writes its progress to stderr. Through npm.ps1 that becomes a
# NativeCommandError, which $ErrorActionPreference = Stop turns into a fatal
# error mid-build; cmd /c keeps it plain output and the exit code is the check.
try { & cmd /c 'npm run build' 2>&1 | Out-Host } finally { Pop-Location }
if ($LASTEXITCODE -ne 0) { Write-Error "npm run build failed ($LASTEXITCODE)"; exit 1 }

Write-Host '==> [2/6] embed ripgrep'
if (Test-Path $rgBin) {
	# Already staged (and stamped in the Makefile's terms): re-downloading 5 MB
	# on every build is pointless. Delete the file to force a refresh.
	Write-Host "    rg already present, skipping download: $rgBin"
} else {
	& (Join-Path $PSScriptRoot 'embed-rg-windows.ps1') -Arch amd64
	if ($LASTEXITCODE -ne 0) { Write-Error 'embed-rg-windows.ps1 failed'; exit 1 }
}
if (-not (Test-Path $rgBin)) { Write-Error "missing embed binary: $rgBin"; exit 1 }
$rgSize = (Get-Item $rgBin).Length
if ($rgSize -lt 1000000) { Write-Error "rg embed binary looks wrong ($rgSize bytes)"; exit 1 }
Write-Host "    rg = $rgSize bytes"

$desktopDir = Join-Path $repo 'cmd/octo-desktop'

Write-Host '==> [3/6] generate-syso (icon + manifest)'
Push-Location $desktopDir
try { & go run ./build/windows/generate-syso amd64 build/windows/icon.ico build/windows/wails.exe.manifest } finally { Pop-Location }
if ($LASTEXITCODE -ne 0) { Write-Error 'generate-syso failed'; exit 1 }

$brandLdflags = "-X github.com/open-octo/octo-agent/internal/brand.Name=$brandName" +
	" -X github.com/open-octo/octo-agent/internal/brand.AppID=$brandAppID" +
	" -X github.com/open-octo/octo-agent/internal/brand.SingleInstanceID=$brandSID" +
	" -X github.com/open-octo/octo-agent/internal/brand.DesktopPort=$brandPort" +
	" -X github.com/open-octo/octo-agent/internal/brand.UpstreamUpdates=$brandUpd" +
	" -X github.com/open-octo/octo-agent/internal/brand.ExeName=$brandExe"

Write-Host "==> [4/6] go build $brandExe"
$env:CGO_ENABLED = '0'
$desktopLdflags = "-H windowsgui -s -w -X github.com/open-octo/octo-agent/internal/version.Version=$Version -X github.com/open-octo/octo-agent/internal/version.Commit=$Commit -X github.com/open-octo/octo-agent/internal/tools/rgembed.version=$RgVersion $brandLdflags"
Push-Location $desktopDir
try { & go build -tags embedrg -ldflags $desktopLdflags -o $desktopExe . } finally { Pop-Location }
if ($LASTEXITCODE -ne 0) { Write-Error 'go build (desktop) failed'; exit 1 }

Write-Host '==> [5/6] go build octo.exe'
$cliLdflags = "-s -w -X github.com/open-octo/octo-agent/internal/version.Version=$Version -X github.com/open-octo/octo-agent/internal/version.Commit=$Commit -X github.com/open-octo/octo-agent/internal/tools/rgembed.version=$RgVersion $brandLdflags"
Push-Location $repo
try { & go build -tags embedrg -ldflags $cliLdflags -o $cliExe ./cmd/octo } finally { Pop-Location }
if ($LASTEXITCODE -ne 0) { Write-Error 'go build (cli) failed'; exit 1 }

# Self-check: subsystem must be 2 (IMAGE_SUBSYSTEM_WINDOWS_GUI). 3 means the
# console window is back, whatever the ldflags claimed.
$bytes = [IO.File]::ReadAllBytes($desktopExe)
$peOffset = [BitConverter]::ToInt32($bytes, 0x3C)
$subsystem = [BitConverter]::ToUInt16($bytes, $peOffset + 0x5C)
Write-Host "==> $brandExe Subsystem=$subsystem (expect 2)"
if ($subsystem -ne 2) {
	Write-Error "PE subsystem is $subsystem, expected 2 (WINDOWS_GUI): -H windowsgui did not reach the linker"
	exit 1
}

# Brand self-check: a missed -X falls back silently to whatever the package
# defaults are, which is how two builds end up sharing one single-instance
# mutex and one port. The strings are plain ASCII in the binary.
$ascii = [Text.Encoding]::ASCII.GetString($bytes)
foreach ($needle in @($brandSID, $brandAppID)) {
	if ($ascii.IndexOf($needle) -lt 0) {
		Write-Error "brand identity '$needle' not found in ${brandExe}: the -X injection did not reach the package"
		exit 1
	}
}
Write-Host "==> brand ok: embedded $brandSID"

Write-Host "==> $brandExe $((Get-Item $desktopExe).Length) bytes"
Write-Host "==> octo.exe  $((Get-Item $cliExe).Length) bytes"

# Portable stage: the deliverable, and the only one. A single self-contained
# GUI exe; it creates Data/ beside itself on first launch, so the USB layout
# needs no archive and no installer. Overwriting the previous copy is the whole
# "publish" step -- nothing else in dist/ is touched or removed here.
Write-Host "==> [6/6] portable stage (single exe, no zip)"
New-Item -ItemType Directory -Force -Path $portableDir | Out-Null
Copy-Item -Path $desktopExe -Destination $portableExe -Force
if (-not (Test-Path $portableExe)) { Write-Error "portable exe missing: $portableExe"; exit 1 }
Write-Host "==> dist/portable/$brandExe $((Get-Item $portableExe).Length) bytes"
Write-Host 'DESKTOP_BUILD_OK'
