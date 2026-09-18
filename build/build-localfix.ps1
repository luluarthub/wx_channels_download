param(
    [string]$Go = 'go',
    [string]$OutputDirectory = ''
)

$ErrorActionPreference = 'Stop'
$projectRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
if (-not $OutputDirectory) { $OutputDirectory = Join-Path $projectRoot 'dist\localfix7' }
$OutputDirectory = [System.IO.Path]::GetFullPath($OutputDirectory)
New-Item -ItemType Directory -Path $OutputDirectory -Force | Out-Null
$oldCgo = $env:CGO_ENABLED
$oldGoos = $env:GOOS
$oldGoarch = $env:GOARCH
try {
    $env:CGO_ENABLED = '0'
    $env:GOOS = 'windows'
    $env:GOARCH = 'amd64'
    Push-Location $projectRoot
    try {
        & $Go build -mod=readonly -buildvcs=false -trimpath -tags 'with_gvisor,embed_inject,sqlite_only,embed_frontend_inject' '-ldflags=-s -w -X main.Mode=release' -o (Join-Path $OutputDirectory 'wx_video_download.exe') .
        if ($LASTEXITCODE -ne 0) { throw 'Go build failed.' }
        $revision = & git -c "safe.directory=$($projectRoot.Replace('\','/'))" rev-parse HEAD
        $status = & git -c "safe.directory=$($projectRoot.Replace('\','/'))" status --porcelain
    } finally { Pop-Location }
    $configTarget = Join-Path $OutputDirectory 'config.yaml'
    if (-not (Test-Path -LiteralPath $configTarget)) {
        Copy-Item -LiteralPath (Join-Path $projectRoot 'internal\config\config.template.yaml') -Destination $configTarget
    }
    Copy-Item -LiteralPath (Join-Path $projectRoot 'LICENSE') -Destination (Join-Path $OutputDirectory 'LICENSE') -Force
    Copy-Item -LiteralPath (Join-Path $projectRoot 'MERGE-NOTES.md') -Destination (Join-Path $OutputDirectory 'MERGE-NOTES.md') -Force
    Get-ChildItem -LiteralPath (Join-Path $PSScriptRoot 'windows') -File | ForEach-Object {
        Copy-Item -LiteralPath $_.FullName -Destination (Join-Path $OutputDirectory $_.Name) -Force
    }
    $binary = Join-Path $OutputDirectory 'wx_video_download.exe'
    [ordered]@{
        version = [regex]::Match((Get-Content -LiteralPath (Join-Path $projectRoot 'main.go') -Raw), 'var AppVer = "([^"]+)"').Groups[1].Value
        sourceCommit = [string]$revision
        sourceModified = [bool]$status
        sha256 = (Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash
        bytes = (Get-Item -LiteralPath $binary).Length
        buildTags = 'with_gvisor,embed_inject,sqlite_only,embed_frontend_inject'
    } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $OutputDirectory 'build-manifest.json') -Encoding UTF8
    Write-Output "Built runnable package: $OutputDirectory"
} finally {
    $env:CGO_ENABLED = $oldCgo
    $env:GOOS = $oldGoos
    $env:GOARCH = $oldGoarch
}
