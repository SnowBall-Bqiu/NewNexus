param(
    [string]$OutputDirectory = ''
)
$ErrorActionPreference = 'Stop'
$projectRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
if (!$OutputDirectory) { $OutputDirectory = Join-Path $projectRoot 'web/dist/agent' }
if (![IO.Path]::IsPathRooted($OutputDirectory)) { $OutputDirectory = Join-Path $projectRoot $OutputDirectory }
$destination = [IO.Path]::GetFullPath($OutputDirectory)
New-Item -ItemType Directory -Path $destination -Force | Out-Null
$oldGoOs = $env:GOOS; $oldGoArch = $env:GOARCH; $oldCgo = $env:CGO_ENABLED
Push-Location $projectRoot
try {
    $env:CGO_ENABLED = '0'; $env:GOOS = 'linux'
    foreach ($architecture in @('amd64', 'arm64')) {
        $env:GOARCH = $architecture
        & go build -trimpath '-ldflags=-s -w' -o (Join-Path $destination "newnexus-agent-linux-$architecture") ./cmd/agent
        if ($LASTEXITCODE) { throw "Agent $architecture download build failed" }
    }
    Write-Output "Linux Agent downloads: $destination"
} finally {
    $env:GOOS = $oldGoOs; $env:GOARCH = $oldGoArch; $env:CGO_ENABLED = $oldCgo
    Pop-Location
}
