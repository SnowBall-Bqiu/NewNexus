param(
    [string]$OutputDirectory = '',
    [switch]$SkipDependencyInstall
)
$ErrorActionPreference = 'Stop'
$projectRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
if (!$OutputDirectory) { $OutputDirectory = Join-Path $projectRoot ('output/release-' + (Get-Date -Format 'yyyyMMdd-HHmmss')) }
$releaseRoot = [IO.Path]::GetFullPath($OutputDirectory)
if (Test-Path -LiteralPath $releaseRoot) { throw 'OutputDirectory must be a new path to avoid overwriting an existing release.' }
New-Item -ItemType Directory -Path $releaseRoot | Out-Null
$oldGoOs = $env:GOOS; $oldGoArch = $env:GOARCH; $oldCgo = $env:CGO_ENABLED
Push-Location $projectRoot
try {
    Push-Location (Join-Path $projectRoot 'web')
    try {
        if (!$SkipDependencyInstall) { & npm ci --registry=https://registry.npmjs.org --no-audit --no-fund; if ($LASTEXITCODE) { throw 'npm ci failed' } }
        & npm run build; if ($LASTEXITCODE) { throw 'Frontend build failed' }
    } finally { Pop-Location }
    & (Join-Path $PSScriptRoot 'build-agent-downloads.ps1')
    $env:CGO_ENABLED = '0'; $env:GOOS = 'linux'
    foreach ($architecture in @('amd64', 'arm64')) {
        $env:GOARCH = $architecture
        $destination = Join-Path $releaseRoot "linux-$architecture"
        New-Item -ItemType Directory -Path $destination | Out-Null
        & go build -trimpath '-ldflags=-s -w' -o (Join-Path $destination 'newnexus-server') ./cmd/server
        if ($LASTEXITCODE) { throw "Server $architecture build failed" }
        Copy-Item -LiteralPath (Join-Path $projectRoot "web/dist/agent/newnexus-agent-linux-$architecture") -Destination (Join-Path $destination 'newnexus-agent')
        Copy-Item -LiteralPath (Join-Path $projectRoot 'web/dist') -Destination (Join-Path $destination 'web') -Recurse
        foreach ($directory in @('docs', 'api')) { Copy-Item -LiteralPath (Join-Path $projectRoot $directory) -Destination (Join-Path $destination $directory) -Recurse }
        foreach ($file in @('deploy/Caddyfile.example', 'deploy/newnexus.env.example', 'deploy/systemd/newnexus.service', 'deploy/tls/README.md')) {
            $target = Join-Path $destination $file
            New-Item -ItemType Directory -Path (Split-Path -Parent $target) -Force | Out-Null
            Copy-Item -LiteralPath (Join-Path $projectRoot $file) -Destination $target
        }
        New-Item -ItemType Directory -Path (Join-Path $destination 'scripts'), (Join-Path $destination 'cmd/agent') | Out-Null
        Copy-Item -LiteralPath (Join-Path $projectRoot 'scripts/sqlite-backup.py') -Destination (Join-Path $destination 'scripts/sqlite-backup.py')
        Copy-Item -LiteralPath (Join-Path $projectRoot 'cmd/agent/newnexus-agent.service') -Destination (Join-Path $destination 'cmd/agent/newnexus-agent.service')
        foreach ($file in @('README.md', 'LICENSE')) { Copy-Item -LiteralPath (Join-Path $projectRoot $file) -Destination (Join-Path $destination $file) }
        $checksums = foreach ($file in @('newnexus-server', 'newnexus-agent', 'web/agent/newnexus-agent-linux-amd64', 'web/agent/newnexus-agent-linux-arm64')) { ((Get-FileHash -LiteralPath (Join-Path $destination $file) -Algorithm SHA256).Hash.ToLowerInvariant()) + '  ' + $file }
        Set-Content -LiteralPath (Join-Path $destination 'SHA256SUMS') -Value $checksums -Encoding ascii
    }
    Write-Output "Linux release directories: $releaseRoot"
} finally {
    $env:GOOS = $oldGoOs; $env:GOARCH = $oldGoArch; $env:CGO_ENABLED = $oldCgo
    Pop-Location
}
