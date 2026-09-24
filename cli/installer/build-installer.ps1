#Requires -Version 5.1
<#
.SYNOPSIS
  Build Glue NSIS installer(s) for Windows amd64 and/or arm64.

.EXAMPLE
  .\build-installer.ps1
  .\build-installer.ps1 -Version 0.1.10
  .\build-installer.ps1 -Arch arm64
  .\build-installer.ps1 -Arch amd64,arm64 -Version 0.1.10
#>
param(
    [ValidateSet('amd64', 'arm64')]
    [string[]]$Arch = @('amd64', 'arm64'),

    [string]$Version = '',
    [string]$GlueExe = '',
    [string]$ShimExe = '',
    [string]$ShimReleaseUrl = '',
    [string]$DepsBase = $(if ($env:GLUE_DEPS_BASE) { $env:GLUE_DEPS_BASE.TrimEnd('/') } else { 'https://gluestick.sh/scripts' }),
    [switch]$SkipBuildGlue,
    [switch]$SkipSha256
)

$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$installerDir = $PSScriptRoot
$repoRoot = Split-Path $installerDir -Parent
$Script:DownloadHeaders = @{
    'User-Agent' = 'Glue-Installer/1.0 (Windows; gluestick.sh)'
}

function Get-DefaultVersion {
    $versionGo = Join-Path $repoRoot 'version\version.go'
    if (-not (Test-Path -LiteralPath $versionGo)) { return '0.0.0-dev' }
    $content = Get-Content -LiteralPath $versionGo -Raw
    if ($content -match 'Version\s*=\s*"([^"]+)"') { return $Matches[1] }
    return '0.0.0-dev'
}

function Find-Makensis {
    $candidates = @(
        "${env:ProgramFiles(x86)}\NSIS\makensis.exe",
        "$env:ProgramFiles\NSIS\makensis.exe",
        "${env:ProgramFiles(x86)}\NSIS\Bin\makensis.exe"
    )
    foreach ($path in $candidates) {
        if (Test-Path -LiteralPath $path) { return $path }
    }
    throw @"
NSIS not found. Install it first, for example:
  choco install nsis -y
"@
}

function Normalize-Sha256Hex {
    param([string]$Value)
    if (-not $Value) { return '' }
    $Value = $Value.Trim().ToLowerInvariant() -replace '^sha256:', ''
    if ($Value -match '^[0-9a-f]{64}$') { return $Value }
    return ''
}

function Test-FileSha256 {
    param([string]$Path, [string]$Expected)
    $expectedNorm = Normalize-Sha256Hex $Expected
    if (-not $expectedNorm) { return }
    $actual = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $expectedNorm) {
        throw "SHA256 mismatch for $(Split-Path -Leaf $Path): expected $expectedNorm got $actual"
    }
}

function Test-LooksLikeHtml {
    param([string]$Text)
    if (-not $Text) { return $false }
    $trim = $Text.TrimStart()
    return $trim.StartsWith('<') -or $trim.StartsWith('<!')
}

function Resolve-DepsContext {
    param([string]$Base)

    $localCandidates = @()
    if ($Base -and $Base -notmatch '^https?://') {
        $localCandidates += Join-Path $Base 'deps'
    }
    $localCandidates += Join-Path $installerDir 'deps'

    foreach ($root in $localCandidates) {
        $manifestPath = Join-Path $root 'manifest.json'
        if (Test-Path -LiteralPath $manifestPath) {
            return @{
                Mode       = 'local'
                LocalRoot  = (Resolve-Path -LiteralPath $root).Path
                RemoteBase = 'https://gluestick.sh/scripts'
            }
        }
    }

    $remoteBase = if ($Base) { $Base.TrimEnd('/') } else { 'https://gluestick.sh/scripts' }
    return @{
        Mode       = 'remote'
        LocalRoot  = ''
        RemoteBase = $remoteBase
    }
}

function Get-DepsManifest {
    param($DepsContext)

    if ($DepsContext.Mode -eq 'local') {
        $manifestPath = Join-Path $DepsContext.LocalRoot 'manifest.json'
        Write-Host "Using local manifest: $manifestPath"
        return Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
    }

    $manifestUrl = "$($DepsContext.RemoteBase)/deps/manifest.json"
    Write-Host "Fetching manifest: $manifestUrl"
    return Invoke-DownloadWithRetry -Uri $manifestUrl | ConvertFrom-Json
}

function Copy-DepsAsset {
    param(
        $DepsContext,
        [string]$TargetArch,
        [string]$FileName,
        [string]$OutFile
    )

    if ($DepsContext.Mode -eq 'local') {
        $source = Join-Path (Join-Path $DepsContext.LocalRoot $TargetArch) $FileName
        if (-not (Test-Path -LiteralPath $source)) {
            throw "Local deps asset not found: $source"
        }
        Write-Host "  copy $source"
        Copy-Item -LiteralPath $source -Destination $OutFile -Force
        return
    }

    $sourceUrl = "$($DepsContext.RemoteBase)/deps/$TargetArch/$FileName"
    Write-Host "  download $sourceUrl"
    Invoke-DownloadWithRetry -Uri $sourceUrl -OutFile $OutFile
}

function Invoke-DownloadWithRetry {
    param(
        [Parameter(Mandatory)]
        [string]$Uri,

        [string]$OutFile = '',
        [int]$MaxAttempts = 3,
        [int]$TimeoutSec = 600
    )

    $attempt = 0
    while ($true) {
        $attempt++
        try {
            if ($OutFile) {
                Invoke-WebRequest -Uri $Uri -OutFile $OutFile -UseBasicParsing -TimeoutSec $TimeoutSec -Headers $Script:DownloadHeaders
                if (-not (Test-Path -LiteralPath $OutFile) -or (Get-Item -LiteralPath $OutFile).Length -eq 0) {
                    throw 'download produced empty file'
                }
                $preview = Get-Content -LiteralPath $OutFile -TotalCount 1 -ErrorAction SilentlyContinue
                if (Test-LooksLikeHtml $preview) {
                    throw 'server returned HTML instead of file content'
                }
                return
            }

            $response = Invoke-WebRequest -Uri $Uri -UseBasicParsing -TimeoutSec $TimeoutSec -Headers $Script:DownloadHeaders
            if ($response.StatusCode -ge 400) {
                throw "HTTP $($response.StatusCode)"
            }
            if (Test-LooksLikeHtml $response.Content) {
                throw 'server returned HTML instead of expected text'
            }
            return $response.Content
        } catch {
            if ($attempt -ge $MaxAttempts) { throw }
            $delay = [Math]::Min(30, 5 * $attempt)
            Write-Warning "Download failed (attempt $attempt/$MaxAttempts): $Uri -- $_"
            Start-Sleep -Seconds $delay
            if ($OutFile) {
                Remove-Item -LiteralPath $OutFile -Force -ErrorAction SilentlyContinue
            }
        }
    }
}

function Prepare-Payload {
    param(
        [string]$TargetArch,
        [string]$GluePath,
        [string]$ShimPath,
        [string]$DepsRoot
    )

    $depsContext = Resolve-DepsContext -Base $DepsRoot
    $outputDir = Join-Path $installerDir "payload\$TargetArch"
    $manifest = Get-DepsManifest -DepsContext $depsContext
    $archManifest = $manifest.architectures.$TargetArch
    if (-not $archManifest) {
        throw "No dependency manifest for architecture: $TargetArch"
    }

    if (Test-Path -LiteralPath $outputDir) {
        Remove-Item -LiteralPath $outputDir -Recurse -Force
    }
    $binDir = Join-Path $outputDir 'bin'
    $null = New-Item -ItemType Directory -Force -Path $binDir

    Write-Host "Copying glue.exe and shim.exe -> $outputDir"
    Copy-Item -LiteralPath $GluePath -Destination (Join-Path $outputDir 'glue.exe') -Force
    Copy-Item -LiteralPath $ShimPath -Destination (Join-Path $outputDir 'shim.exe') -Force

    $tempDir = Join-Path $env:TEMP "glue-payload-$TargetArch"
    if (Test-Path -LiteralPath $tempDir) {
        Remove-Item -LiteralPath $tempDir -Recurse -Force
    }
    $null = New-Item -ItemType Directory -Force -Path $tempDir

    try {
        foreach ($asset in $archManifest.files) {
            $destPath = Join-Path $outputDir ($asset.dest -replace '/', '\')
            $tempFile = Join-Path $tempDir $asset.file

            Copy-DepsAsset -DepsContext $depsContext -TargetArch $TargetArch -FileName $asset.file -OutFile $tempFile
            Test-FileSha256 -Path $tempFile -Expected $asset.sha256

            if ($asset.extract) {
                $destPath = Join-Path $binDir $asset.file
            }
            $destParent = Split-Path -Parent $destPath
            if ($destParent) {
                $null = New-Item -ItemType Directory -Force -Path $destParent
            }
            Move-Item -LiteralPath $tempFile -Destination $destPath -Force
        }
    } finally {
        Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue
    }

    $sevenZ = Join-Path $binDir '7z.exe'
    if (-not (Test-Path -LiteralPath $sevenZ)) {
        throw "7z.exe missing in payload"
    }

    Write-Host "Payload ready: $outputDir"
}

function Ensure-GlueExe {
    param(
        [string]$TargetArch,
        [string]$Path,
        [string]$Ver,
        [switch]$SkipBuild
    )
    if ($Path) {
        if (-not (Test-Path -LiteralPath $Path)) {
            throw "Glue binary not found: $Path"
        }
        return (Resolve-Path -LiteralPath $Path).Path
    }

    $default = Join-Path $repoRoot "glue-windows-$TargetArch.exe"
    if (-not $SkipBuild -or -not (Test-Path -LiteralPath $default)) {
        if ($SkipBuild) {
            throw "Glue binary not found: $default (pass -GlueExe or drop -SkipBuildGlue)"
        }
        Write-Host "Building glue.exe ($TargetArch)..." -ForegroundColor Cyan
        Push-Location $repoRoot
        try {
            $env:GOOS = 'windows'
            $env:GOARCH = $TargetArch
            $env:CGO_ENABLED = '0'
            $date = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
            $commit = ''
            try { $commit = (git rev-parse HEAD).Substring(0, 12) } catch { }
            go build -trimpath `
                -ldflags "-s -w -X github.com/gluestick-sh/cli/version.Version=$Ver -X github.com/gluestick-sh/cli/version.Commit=$commit -X github.com/gluestick-sh/cli/version.Date=$date" `
                -o $default ./glue
        } finally {
            Pop-Location
            Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED -ErrorAction SilentlyContinue
        }
    }
    return (Resolve-Path -LiteralPath $default).Path
}

function Ensure-ShimExe {
    param(
        [string]$TargetArch,
        [string]$Path,
        [string]$ReleaseUrl
    )
    if ($Path -and (Test-Path -LiteralPath $Path)) {
        return (Resolve-Path -LiteralPath $Path).Path
    }

    $default = Join-Path $installerDir "shim-windows-$TargetArch.exe"
    if (Test-Path -LiteralPath $default) {
        return (Resolve-Path -LiteralPath $default).Path
    }

    $url = $ReleaseUrl
    if (-not $url) {
        $url = "https://github.com/gluestick-sh/shim/releases/latest/download/shim-windows-$TargetArch.exe"
    }
    Write-Host "Downloading shim.exe ($TargetArch)..." -ForegroundColor Cyan
    Write-Host "  $url" -ForegroundColor DarkGray
    Invoke-DownloadWithRetry -Uri $url -OutFile $default
    return (Resolve-Path -LiteralPath $default).Path
}

function Build-InstallerForArch {
    param(
        [string]$TargetArch,
        [string]$Ver,
        [string]$Makensis,
        [string]$GluePathOverride,
        [string]$ShimPathOverride,
        [string]$ShimUrl,
        [string]$DepsRoot,
        [switch]$SkipBuild,
        [switch]$SkipHash
    )

    $gluePath = Ensure-GlueExe -TargetArch $TargetArch -Path $GluePathOverride -Ver $Ver -SkipBuild:$SkipBuild
    $shimPath = Ensure-ShimExe -TargetArch $TargetArch -Path $ShimPathOverride -ReleaseUrl $ShimUrl

    Write-Host "Preparing payload ($TargetArch)..." -ForegroundColor Cyan
    Prepare-Payload -TargetArch $TargetArch -GluePath $gluePath -ShimPath $shimPath -DepsRoot $DepsRoot

    $outputDir = Join-Path $installerDir 'output'
    $null = New-Item -ItemType Directory -Force -Path $outputDir
    $setupName = "GlueSetup-$TargetArch.exe"
    $setupPath = Join-Path $outputDir $setupName

    Write-Host "Compiling $setupName (version $Ver)..." -ForegroundColor Cyan
    Push-Location $installerDir
    try {
        & $Makensis "/DPAYLOAD_VERSION=$Ver" "/DPAYLOAD_ARCH=$TargetArch" "Glue.nsi" | Out-Host
        if ($LASTEXITCODE -ne 0) {
            throw "makensis failed with exit code $LASTEXITCODE"
        }
    } finally {
        Pop-Location
    }

    if (-not (Test-Path -LiteralPath $setupPath)) {
        throw "Installer not produced: $setupPath"
    }

    if (-not $SkipHash) {
        $hash = (Get-FileHash -LiteralPath $setupPath -Algorithm SHA256).Hash.ToLower()
        Set-Content -LiteralPath "$setupPath.sha256" -Value $hash -NoNewline
        Write-Host "SHA256: $hash" -ForegroundColor DarkGray
    }

    Write-Host "Built: $setupPath" -ForegroundColor Green
    return ,$setupPath
}

if (-not $Version) {
    $Version = Get-DefaultVersion
}

$makensis = Find-Makensis
$built = @()
$singleArch = ($Arch.Count -eq 1)
foreach ($targetArch in $Arch) {
    if (-not $singleArch) {
        Write-Host ""
        Write-Host "=== $targetArch ===" -ForegroundColor Cyan
    }
    $built += Build-InstallerForArch `
        -TargetArch $targetArch `
        -Ver $Version `
        -Makensis $makensis `
        -GluePathOverride $(if ($singleArch) { $GlueExe } else { '' }) `
        -ShimPathOverride $(if ($singleArch) { $ShimExe } else { '' }) `
        -ShimUrl $ShimReleaseUrl `
        -DepsRoot $DepsBase `
        -SkipBuild:$SkipBuildGlue `
        -SkipHash:$SkipSha256
}

Write-Host ""
Write-Host ("Done. {0} installer(s):" -f $built.Count) -ForegroundColor Green
$built | ForEach-Object { Write-Host "  $_" }
