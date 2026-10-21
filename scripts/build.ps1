#!/usr/bin/env pwsh
#requires -Version 7.0
<#
.SYNOPSIS
Builds the three linux/amd64 binaries and the release metadata for one commit.

.DESCRIPTION
Runs only on a clean, committed, pushed worktree, and refuses to overwrite an
existing dist directory. The output is

  O:\WinCache\nahook\dist\<8hex>\nahook
  O:\WinCache\nahook\dist\<8hex>\nahookctl
  O:\WinCache\nahook\dist\<8hex>\mockrelay
  O:\WinCache\nahook\dist\<8hex>\BUILDINFO.txt
  O:\WinCache\nahook\dist\<8hex>\SHA256SUMS

BUILDINFO.txt and SHA256SUMS are ASCII with LF endings so that sha256sum -c on
Linux reads them unchanged; PowerShell's default text cmdlets would write CRLF
(design §7, T8).
#>
[CmdletBinding()]
param(
    # Where the per-commit dist directories live. The default is the Win11
    # layout of the task book; an audit run passes a scratch directory.
    [string]$DistRoot = 'O:\WinCache\nahook\dist'
)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false

# The repository is the one this script belongs to, so a separate clone builds
# itself and never O:\NaHook by accident.
$Repo = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$GoBin = 'O:\WinCache\tools\go\bin'
$GccBin = 'O:\WinCache\tools\msys64\ucrt64\bin'
$TmpDir = 'O:\WinCache\tmp'

$script:dist = $null
# created is set only once this run has made the dist directory itself. A
# failure removes that directory and nothing else; a dist directory that
# already existed is never touched.
$script:created = $null

function Stop-Build([string]$Message) {
    Write-Host "build: $Message" -ForegroundColor Red
    if ($script:created -and (Test-Path -LiteralPath $script:created)) {
        Remove-Item -LiteralPath $script:created -Recurse -Force -ErrorAction SilentlyContinue
        Write-Host "build: removed incomplete $script:created" -ForegroundColor Yellow
    }
    exit 1
}

Set-Location $Repo

# 1. Only a committed, pushed, clean worktree may be built.
$dirty = @(git status --porcelain)
if ($LASTEXITCODE -ne 0) { Stop-Build 'git status failed' }
if ($dirty.Count -gt 0) { Stop-Build ("working tree is not clean:`n" + ($dirty -join "`n")) }

$branch = (git rev-parse --abbrev-ref HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or $branch -eq 'HEAD') { Stop-Build 'not on a branch' }
$commit = (git rev-parse HEAD).Trim()
$pushed = git rev-parse "origin/$branch" 2>$null
if ($LASTEXITCODE -ne 0) { Stop-Build "branch $branch has no origin counterpart" }
$pushed = "$pushed".Trim()
if ($commit -ne $pushed) { Stop-Build "HEAD $commit is not pushed to origin/$branch ($pushed)" }

$short = $commit.Substring(0, 8)
$script:dist = Join-Path $DistRoot $short
if (Test-Path -LiteralPath $script:dist) {
    Stop-Build "refusing to overwrite existing $script:dist"
}

New-Item -ItemType Directory -Path $script:dist | Out-Null
$script:created = $script:dist

# 2. Cross-compile the three commands (design §7).
$env:PATH = "$GoBin;$GccBin;" + $env:PATH
$env:GOTOOLCHAIN = 'local'
$env:GOTMPDIR = $TmpDir
$env:CGO_ENABLED = '0'
$env:GOOS = 'linux'
$env:GOARCH = 'amd64'

# A trailing forward slash makes -o a directory for all packages; a trailing
# backslash would be mangled by the Windows command-line quoting rules.
$outDir = ($script:dist -replace '\\', '/') + '/'
& go build -trimpath -o $outDir ./cmd/...
$buildExit = $LASTEXITCODE

Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
if ($buildExit -ne 0) { Stop-Build 'go build failed' }

foreach ($name in 'nahook', 'nahookctl', 'mockrelay') {
    if (-not (Test-Path -LiteralPath (Join-Path $script:dist $name))) {
        Stop-Build "go build did not produce $name"
    }
}

# 3. BUILDINFO.txt: commit, toolchain, target, build command, UTC time.
$goVersion = (& go version).Trim()
$builtAt = (Get-Date).ToUniversalTime().ToString("yyyy-MM-dd'T'HH:mm:ss'Z'")
$buildCommand = 'go build -trimpath -o <dist>\ ./cmd/...'
$info = @(
    "commit: $commit"
    "go_version: $goVersion"
    "goos: linux"
    "goarch: amd64"
    "cgo_enabled: 0"
    "build: $buildCommand"
    "built_at_utc: $builtAt"
) -join "`n"
[IO.File]::WriteAllText((Join-Path $script:dist 'BUILDINFO.txt'), $info + "`n", [Text.ASCIIEncoding]::new())

# 4. SHA256SUMS: four entries, "<hash>  <name>", LF.
$lines = foreach ($name in 'nahook', 'nahookctl', 'mockrelay', 'BUILDINFO.txt') {
    $hash = (Get-FileHash -LiteralPath (Join-Path $script:dist $name) -Algorithm SHA256).Hash.ToLowerInvariant()
    "$hash  $name"
}
[IO.File]::WriteAllText((Join-Path $script:dist 'SHA256SUMS'), (($lines -join "`n") + "`n"), [Text.ASCIIEncoding]::new())

Write-Host "build: $short -> $script:dist"
Get-Content -LiteralPath (Join-Path $script:dist 'BUILDINFO.txt')
Get-Content -LiteralPath (Join-Path $script:dist 'SHA256SUMS')
exit 0
