#!/usr/bin/env pwsh
#requires -Version 7.0
<#
.SYNOPSIS
Publishes the dist directory of one commit as a GitHub prerelease.

.DESCRIPTION
Refuses when the worktree is dirty, when HEAD is not pushed, when the dist
directory is missing, or when the tag build-<8hex> already exists — one commit
gets exactly one release (design §7, T8). The five attachments come from
O:\WinCache\nahook\dist\<8hex>\ and are never copied into the repository.
#>
[CmdletBinding()]
param(
    [string]$DistRoot = 'O:\WinCache\nahook\dist'
)

$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false

$Repo = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$GitHubRepo = 'ARYYDS/NaHook'

function Stop-Release([string]$Message) {
    Write-Host "release: $Message" -ForegroundColor Red
    exit 1
}

Set-Location $Repo

$dirty = @(git status --porcelain)
if ($LASTEXITCODE -ne 0) { Stop-Release 'git status failed' }
if ($dirty.Count -gt 0) { Stop-Release ("working tree is not clean:`n" + ($dirty -join "`n")) }

$branch = (git rev-parse --abbrev-ref HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or $branch -eq 'HEAD') { Stop-Release 'not on a branch' }
$commit = (git rev-parse HEAD).Trim()
$pushed = git rev-parse "origin/$branch" 2>$null
if ($LASTEXITCODE -ne 0) { Stop-Release "branch $branch has no origin counterpart" }
$pushed = "$pushed".Trim()
if ($commit -ne $pushed) { Stop-Release "HEAD $commit is not pushed to origin/$branch ($pushed)" }

$short = $commit.Substring(0, 8)
$tag = "build-$short"
$dist = Join-Path $DistRoot $short

$attachments = 'nahook', 'nahookctl', 'mockrelay', 'BUILDINFO.txt', 'SHA256SUMS'
foreach ($name in $attachments) {
    if (-not (Test-Path -LiteralPath (Join-Path $dist $name))) {
        Stop-Release "missing $dist\$name; run scripts/build.ps1 first"
    }
}

# The dist directory must belong to this commit and still match its checksums:
# what is uploaded is exactly what build.ps1 produced for HEAD.
$infoCommit = (Select-String -LiteralPath (Join-Path $dist 'BUILDINFO.txt') -Pattern '^commit: ([0-9a-f]{40})$').Matches |
    ForEach-Object { $_.Groups[1].Value } | Select-Object -First 1
if ($infoCommit -ne $commit) { Stop-Release "BUILDINFO.txt is for $infoCommit, not HEAD $commit" }
foreach ($line in [IO.File]::ReadAllLines((Join-Path $dist 'SHA256SUMS'))) {
    if ($line -notmatch '^([0-9a-f]{64})  (\S+)$') { Stop-Release "malformed SHA256SUMS line: $line" }
    $want, $name = $Matches[1], $Matches[2]
    $got = (Get-FileHash -LiteralPath (Join-Path $dist $name) -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($got -ne $want) { Stop-Release "$name does not match SHA256SUMS" }
}

# A tag without a release would still make the create fail, but this reports it
# as the duplicate it is.
$remoteTag = git ls-remote --tags origin "refs/tags/$tag" 2>$null
if ($LASTEXITCODE -ne 0) { Stop-Release 'cannot list remote tags' }
if ("$remoteTag".Trim()) { Stop-Release "tag $tag already exists on GitHub" }

gh release view $tag --json tagName 1>$null 2>$null
if ($LASTEXITCODE -eq 0) { Stop-Release "release $tag already exists" }

# A missing release and a broken gh session both exit non-zero above, so prove
# the repository is actually reachable before creating anything.
gh api "repos/$GitHubRepo" --jq .full_name 1>$null 2>$null
if ($LASTEXITCODE -ne 0) { Stop-Release 'gh cannot reach the GitHub repository' }

$paths = foreach ($name in $attachments) { Join-Path $dist $name }
$output = gh release create $tag --target $commit --prerelease --title "build $short" `
    --notes-file (Join-Path $dist 'BUILDINFO.txt') @paths 2>&1
if ($LASTEXITCODE -ne 0) {
    Write-Host ($output -join "`n")
    Stop-Release 'gh release create failed'
}

Write-Host ($output -join "`n")
$url = (gh release view $tag --json url --jq .url).Trim()
Write-Host "release: $url"
exit 0
