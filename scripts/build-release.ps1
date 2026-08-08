param(
    [string]$Version = "0.1.0",
    [string]$OutputDirectory = "dist"
)

$ErrorActionPreference = "Stop"
$repository = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$output = [System.IO.Path]::GetFullPath((Join-Path $repository $OutputDirectory))
if (-not $output.StartsWith($repository, [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "Output directory must stay inside the repository"
}

Push-Location $repository
try {
    Push-Location "web"
    try {
        npm run build
        if ($LASTEXITCODE -ne 0) {
            throw "Frontend build failed with exit code $LASTEXITCODE"
        }
    }
    finally {
        Pop-Location
    }

    New-Item -ItemType Directory -Force -Path $output | Out-Null
    $targets = @("amd64")
    $archives = @()

    foreach ($architecture in $targets) {
        $name = "newnexus-v$Version-linux-$architecture"
        $stage = Join-Path $output $name
        $resolvedOutputPrefix = $output.TrimEnd([System.IO.Path]::DirectorySeparatorChar) + [System.IO.Path]::DirectorySeparatorChar
        if (-not $stage.StartsWith($resolvedOutputPrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
            throw "Unsafe staging path: $stage"
        }
        if (Test-Path -LiteralPath $stage) {
            Remove-Item -Recurse -Force -LiteralPath $stage
        }
        New-Item -ItemType Directory -Force -Path $stage | Out-Null
        New-Item -ItemType Directory -Force -Path (Join-Path $stage "web") | Out-Null

        $env:CGO_ENABLED = "0"
        $env:GOOS = "linux"
        $env:GOARCH = $architecture
        go build -trimpath -ldflags="-s -w" -o (Join-Path $stage "newnexus-server") ./cmd/server
        if ($LASTEXITCODE -ne 0) {
            throw "Server build failed for linux/$architecture with exit code $LASTEXITCODE"
        }
        go build -trimpath -ldflags="-s -w -X main.version=$Version" -o (Join-Path $stage "newnexus-agent") ./cmd/agent
        if ($LASTEXITCODE -ne 0) {
            throw "Agent build failed for linux/$architecture with exit code $LASTEXITCODE"
        }

        Copy-Item -Recurse -LiteralPath "web\dist" -Destination (Join-Path $stage "web\dist")
        Copy-Item -Recurse -LiteralPath "api" -Destination (Join-Path $stage "api")
        Copy-Item -Recurse -LiteralPath "deploy" -Destination (Join-Path $stage "deploy")
        Copy-Item -Recurse -LiteralPath "docs" -Destination (Join-Path $stage "docs")
        Copy-Item -LiteralPath "README.md", "LICENSE" -Destination $stage

        $archive = Join-Path $output "$name.tar.gz"
        if (Test-Path -LiteralPath $archive) {
            Remove-Item -Force -LiteralPath $archive
        }
        tar -C $output -czf $archive $name
        if ($LASTEXITCODE -ne 0) {
            throw "Archive creation failed for $name with exit code $LASTEXITCODE"
        }
        $archives += $archive
    }

    $checksumPath = Join-Path $output "SHA256SUMS"
    $lines = foreach ($archive in $archives) {
        $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $archive).Hash.ToLowerInvariant()
        "$hash  $([System.IO.Path]::GetFileName($archive))"
    }
    [System.IO.File]::WriteAllLines($checksumPath, $lines, [System.Text.UTF8Encoding]::new($false))
    Write-Output "Release artifacts written to $output"
    $artifacts = @($archives) + @($checksumPath)
    Get-Item -LiteralPath $artifacts | Select-Object Name, Length, LastWriteTime
}
finally {
    Pop-Location
}
