Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$repoRoot = Split-Path -Parent $PSScriptRoot
$origPath = $env:PATH
$origGOOS = [Environment]::GetEnvironmentVariable("GOOS", "Process")
$origGOARCH = [Environment]::GetEnvironmentVariable("GOARCH", "Process")
$origCGO = [Environment]::GetEnvironmentVariable("CGO_ENABLED", "Process")

function Format-MiB {
    param([int64]$Bytes)
    return "{0:N2}" -f ($Bytes / 1MB)
}

function Get-GoPackageBucket {
    param([string]$SymbolName)
    if ($SymbolName -match '^(type:|go:)') {
        $dot = $SymbolName.IndexOf('.')
        if ($dot -gt 0) {
            return $SymbolName.Substring(0, $dot)
        }
        return $SymbolName
    }
    $lastSlash = $SymbolName.LastIndexOf('/')
    if ($lastSlash -ge 0) {
        $dot = $SymbolName.IndexOf('.', $lastSlash)
        if ($dot -gt $lastSlash) {
            return $SymbolName.Substring(0, $dot)
        }
        return $SymbolName
    }
    $firstDot = $SymbolName.IndexOf('.')
    if ($firstDot -gt 0) {
        return $SymbolName.Substring(0, $firstDot)
    }
    return "(other)"
}

Push-Location $repoRoot
$probeExe = $null
try {
    $goCmd = Get-Command go -ErrorAction SilentlyContinue
    $gofmtCmd = Get-Command gofmt -ErrorAction SilentlyContinue
    if (-not $goCmd -or -not $gofmtCmd) {
        Write-Error "go and gofmt must be installed and available in PATH."
        exit 1
    }

    if (-not (Get-Command gcc -ErrorAction SilentlyContinue)) {
        $msysGccBin = "C:\msys64\mingw64\bin"
        if (Test-Path (Join-Path $msysGccBin "gcc.exe")) {
            $env:PATH = "$msysGccBin;$env:PATH"
        } else {
            Write-Error "gcc was not found in PATH or $msysGccBin (required for go test -race)."
            exit 1
        }
    }

    Write-Host "==> [1/4] Checking formatting (gofmt -l .)..." -ForegroundColor Cyan
    $unformatted = @(gofmt -l .)
    if ($LASTEXITCODE -ne 0) {
        throw "gofmt failed with exit code $LASTEXITCODE"
    }
    $unformatted = @($unformatted | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
    if ($unformatted.Count -gt 0) {
        Write-Host "Unformatted Go files:" -ForegroundColor Red
        $unformatted | ForEach-Object { Write-Host "  $_" -ForegroundColor Red }
        throw "gofmt check failed. Run 'gofmt -w .' first."
    }
    Write-Host "    gofmt clean." -ForegroundColor Green

    Write-Host "==> [2/4] Running go vet (GOOS=windows GOARCH=amd64)..." -ForegroundColor Cyan
    $env:GOOS = "windows"
    $env:GOARCH = "amd64"
    $env:CGO_ENABLED = "0"
    & go vet ./...
    if ($LASTEXITCODE -ne 0) {
        throw "go vet failed with exit code $LASTEXITCODE"
    }
    Write-Host "    go vet passed." -ForegroundColor Green

    Write-Host "==> [3/4] Running race tests (go test -race -count=1 ./...)..." -ForegroundColor Cyan
    $env:CGO_ENABLED = "1"
    & go test -race -count=1 ./...
    if ($LASTEXITCODE -ne 0) {
        throw "go test -race failed with exit code $LASTEXITCODE"
    }
    Write-Host "    all tests passed." -ForegroundColor Green

    Write-Host "==> [4/4] Building release binary (AniDownloader.exe)..." -ForegroundColor Cyan
    $env:CGO_ENABLED = "0"
    & go build -trimpath -ldflags="-H=windowsgui -s -w" -o AniDownloader.exe .
    if ($LASTEXITCODE -ne 0) {
        throw "go build release failed with exit code $LASTEXITCODE"
    }

    $gzItem = Get-Item "bin_embed\ffmpeg.exe.gz"
    $exeItem = Get-Item "AniDownloader.exe"

    Write-Host ""
    Write-Host "=== Release Artifact Sizes ===" -ForegroundColor Yellow
    Write-Host ("  bin_embed\ffmpeg.exe.gz : {0,12:N0} bytes ({1} MiB)" -f $gzItem.Length, (Format-MiB $gzItem.Length))
    Write-Host ("  AniDownloader.exe       : {0,12:N0} bytes ({1} MiB)" -f $exeItem.Length, (Format-MiB $exeItem.Length))

    Write-Host ""
    Write-Host "==> Analyzing top Go packages & symbols (via temporary unstripped probe)..." -ForegroundColor Cyan
    $probeExe = Join-Path $env:TEMP ("anidl_probe_{0}.exe" -f $PID)
    & go build -trimpath -ldflags="-H=windowsgui" -o $probeExe .
    if ($LASTEXITCODE -ne 0) {
        throw "probe go build failed with exit code $LASTEXITCODE"
    }

    $nmLines = & go tool nm -size $probeExe
    if ($LASTEXITCODE -ne 0) {
        throw "go tool nm failed with exit code $LASTEXITCODE"
    }

    $pkgSizes = @{}
    $symbols = New-Object System.Collections.Generic.List[object]
    foreach ($line in $nmLines) {
        if ($line -match '^\s*[0-9a-fA-F]+\s+(\d+)\s+(\S+)\s+(.+)$') {
            $sz = [int64]$Matches[1]
            $symType = $Matches[2]
            $symName = $Matches[3]
            # Exclude BSS ('B'/'b') and undefined ('U') since they occupy 0 bytes on disk.
            if ($symType -in @('B', 'b', 'U')) {
                continue
            }
            $bucket = Get-GoPackageBucket $symName
            if ($pkgSizes.ContainsKey($bucket)) {
                $pkgSizes[$bucket] += $sz
            } else {
                $pkgSizes[$bucket] = $sz
            }
            $symbols.Add([PSCustomObject]@{
                Size = $sz
                Type = $symType
                Name = $symName
            })
        }
    }

    Write-Host ""
    Write-Host "=== Top 15 Go Packages / Buckets (Non-BSS Symbol Size) ===" -ForegroundColor Yellow
    $pkgSizes.GetEnumerator() |
        Sort-Object Value -Descending |
        Select-Object -First 15 |
        ForEach-Object {
            [PSCustomObject]@{
                Bytes   = ("{0,10:N0}" -f $_.Value)
                MiB     = (Format-MiB $_.Value)
                Package = $_.Key
            }
        } | Format-Table -AutoSize

    Write-Host "=== Top 15 Largest Symbols (Non-BSS) ===" -ForegroundColor Yellow
    $symbols |
        Sort-Object Size -Descending |
        Select-Object -First 15 |
        ForEach-Object {
            [PSCustomObject]@{
                Bytes  = ("{0,10:N0}" -f $_.Size)
                MiB    = (Format-MiB $_.Size)
                Type   = $_.Type
                Symbol = $_.Name
            }
        } | Format-Table -AutoSize

    Write-Host "Release build and verification succeeded." -ForegroundColor Green
} finally {
    if ($probeExe -and (Test-Path $probeExe)) {
        Remove-Item $probeExe -Force -ErrorAction SilentlyContinue
    }
    $env:PATH = $origPath
    [Environment]::SetEnvironmentVariable("GOOS", $origGOOS, "Process")
    [Environment]::SetEnvironmentVariable("GOARCH", $origGOARCH, "Process")
    [Environment]::SetEnvironmentVariable("CGO_ENABLED", $origCGO, "Process")
    Pop-Location
}
