#Requires -Version 5.1
[CmdletBinding()]
param(
    [ValidateSet('amd64', 'arm64')]
    [string[]]$Arch = @('amd64', 'arm64'),
    [string]$Version = '',
    [switch]$SkipBuild
)

$ErrorActionPreference = 'Stop'
$installerDir = $PSScriptRoot
# The script lives in install\<product>, so the workspace root is two levels up.
$repoRoot = Split-Path -Parent (Split-Path -Parent $installerDir)
$releaseProfile = 'release'

function Get-PinnedToolchain {
    $toolchainFile = Join-Path $repoRoot 'rust-toolchain.toml'
    $content = Get-Content -LiteralPath $toolchainFile -Raw
    if ($content -match '(?m)^\s*channel\s*=\s*"([^"]+)"') { return $Matches[1] }
    throw "Could not read the pinned Rust channel from $toolchainFile."
}

$toolchain = Get-PinnedToolchain

function Get-WorkspaceVersion {
    $manifest = Join-Path $repoRoot 'Cargo.toml'
    $content = Get-Content -LiteralPath $manifest -Raw
    if ($content -match '(?ms)^\[workspace\.package\].*?^\s*version\s*=\s*"([^\"]+)"') {
        return $Matches[1]
    }
    throw 'Could not read [workspace.package].version from Cargo.toml.'
}

function Invoke-Native {
    param([string]$FilePath, [string[]]$Arguments)

    $previous = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        # Merge stderr into the host so PowerShell does not wrap cargo's and
        # makensis' progress lines in NativeCommandError records. With
        # $ErrorActionPreference = 'Stop' (set below) any stderr write from a
        # native command would otherwise abort the build.
        & $FilePath @Arguments 2>&1 | ForEach-Object { Write-Host $_ }
        $code = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previous
    }
    if ($code -ne 0) {
        throw "$FilePath $($Arguments -join ' ') failed with exit code $code."
    }
}

function Find-Makensis {
    $command = Get-Command 'makensis.exe' -CommandType Application -ErrorAction SilentlyContinue |
        Select-Object -First 1
    if ($command) { return $command.Source }

    $candidates = @(
        (Join-Path $env:ProgramFiles 'NSIS\Bin\makensis.exe'),
        (Join-Path ${env:ProgramFiles(x86)} 'NSIS\Bin\makensis.exe')
    )
    foreach ($candidate in $candidates) {
        if ($candidate -and (Test-Path -LiteralPath $candidate)) { return $candidate }
    }
    throw 'NSIS was not found. Install NSIS 3.x or add makensis.exe to PATH.'
}

function Find-Rustup {
    $candidate = Join-Path $HOME '.cargo\bin\rustup.exe'
    if (Test-Path -LiteralPath $candidate) { return $candidate }
    $command = Get-Command 'rustup.exe' -CommandType Application -ErrorAction SilentlyContinue |
        Select-Object -First 1
    if ($command) { return $command.Source }
    throw 'rustup.exe was not found; install Rustup with the MSVC toolchain.'
}

function Find-VcVarsAll {
    $vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio\Installer\vswhere.exe'
    if (Test-Path -LiteralPath $vswhere) {
        $found = & $vswhere -latest -products * -find 'VC\Auxiliary\Build\vcvarsall.bat' 2>$null |
            Select-Object -First 1
        if ($found -and (Test-Path -LiteralPath $found)) { return $found }
    }
    throw 'Visual C++ Build Tools not found (vcvarsall.bat). Install the MSVC C++ build tools.'
}

function Import-MsvcEnvironment {
    param([string]$VcVarsAll, [string]$Architecture)

    $lines = & cmd.exe /c "`"$VcVarsAll`" $Architecture >nul 2>&1 && set"
    if ($LASTEXITCODE -ne 0) {
        throw "vcvarsall.bat $Architecture failed (exit $LASTEXITCODE)."
    }
    foreach ($line in $lines) {
        if ($line -match '^([^=]+)=(.*)$') {
            Set-Item -Path ("env:" + $Matches[1]) -Value $Matches[2]
        }
    }
}

function Invoke-MsvcBuild {
    param([ValidateSet('amd64', 'arm64')][string]$TargetArch)

    $target = if ($TargetArch -eq 'amd64') { 'x86_64-pc-windows-msvc' } else { 'aarch64-pc-windows-msvc' }
    $vcTarget = if ($TargetArch -eq 'amd64') { 'x64' } else { 'arm64' }
    $processArch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    $vcHost = if ($processArch -eq 'ARM64') { 'arm64' } else { 'x64' }
    $rustup = Find-Rustup
    $buildToolchain = $toolchain
    if ($TargetArch -eq 'amd64') {
        $x64Toolchain = "$toolchain-x86_64-pc-windows-msvc"
        $availableToolchains = & $rustup 'toolchain' 'list'
        if ($LASTEXITCODE -eq 0 -and $availableToolchains -match "^$([regex]::Escape($x64Toolchain))(?:\s|$)") {
            $buildToolchain = $x64Toolchain
        }
    }
    $vcvarsArch = if ($vcHost -eq $vcTarget) { $vcTarget } else { "${vcHost}_${vcTarget}" }
    $targetDirName = "cli-$TargetArch-msvc"
    $targetDir = Join-Path $repoRoot "target\$targetDirName"
    $artifactDir = Join-Path (Join-Path $targetDir $target) $releaseProfile
    # The payload carries two binaries: the CLI and the shim it copies onto PATH.
    $binaries = [pscustomobject]@{
        Glue = Join-Path $artifactDir 'glue.exe'
        Shim = Join-Path $artifactDir 'shim.exe'
    }

    if ($SkipBuild) {
        foreach ($path in @($binaries.Glue, $binaries.Shim)) {
            if (-not (Test-Path -LiteralPath $path)) {
                throw "-SkipBuild requested but binary is missing: $path"
            }
        }
        return $binaries
    }

    $previousEnvironment = @{}
    Get-ChildItem Env: | ForEach-Object { $previousEnvironment[$_.Name] = $_.Value }
    $env:CARGO_TARGET_DIR = $targetDir
    $env:RUSTUP_TOOLCHAIN = $buildToolchain
    $extraFlags = '-C target-feature=+crt-static'
    $env:RUSTFLAGS = if ($previousRustFlags) { "$previousRustFlags $extraFlags" } else { $extraFlags }

    try {
        $toolchains = & $rustup 'toolchain' 'list'
        if ($LASTEXITCODE -ne 0 -or -not ($toolchains -match "^$([regex]::Escape($buildToolchain))(?:-|\s)")) {
            throw "Rustup toolchain '$buildToolchain' is missing. Install it with: rustup toolchain install $buildToolchain --profile minimal"
        }

        $installedTargets = & $rustup 'target' 'list' '--installed' '--toolchain' $buildToolchain
        if ($LASTEXITCODE -ne 0 -or $installedTargets -notcontains $target) {
            throw "Rust target '$target' is missing. Install it with: rustup target add $target --toolchain $buildToolchain"
        }

        $vcvars = Find-VcVarsAll
        Import-MsvcEnvironment -VcVarsAll $vcvars -Architecture $vcvarsArch
        $linker = Get-Command 'link.exe' -CommandType Application -ErrorAction SilentlyContinue |
            Select-Object -First 1
        if (-not $linker -or $linker.Source -notmatch "\\$vcTarget\\link\.exe$") {
            throw "MSVC $vcTarget linker not found after vcvarsall $vcvarsArch. Install the matching Visual C++ tools."
        }

        # cargo resolves rustc through PATH. A standalone (non-rustup) Rust install listed
        # earlier in PATH would be used instead of the selected toolchain and fail with
        # E0463 "can't find crate for std" for cross targets, so put this toolchain's bin first.
        $toolchainRustc = & $rustup 'which' 'rustc' '--toolchain' $buildToolchain
        if ($LASTEXITCODE -ne 0 -or -not $toolchainRustc) {
            throw "Could not resolve rustc for toolchain '$buildToolchain': $toolchainRustc"
        }
        $env:PATH = "$(Split-Path -Parent $toolchainRustc);$env:PATH"

        Write-Host "Building glue.exe and shim.exe for $target with MSVC ($buildToolchain; $vcvarsArch)..." -ForegroundColor Cyan
        Invoke-Native -FilePath $rustup -Arguments @(
            'run', $buildToolchain, 'cargo', 'build', '--release',
            '--package', 'gluestick-cli', '--package', 'gluestick-shim', '--target', $target
        )
    } finally {
        Get-ChildItem Env: | Where-Object { -not $previousEnvironment.ContainsKey($_.Name) } |
            ForEach-Object { Remove-Item -LiteralPath "Env:$($_.Name)" -ErrorAction SilentlyContinue }
        foreach ($name in $previousEnvironment.Keys) {
            Set-Item -Path "Env:$name" -Value $previousEnvironment[$name]
        }
    }

    foreach ($path in @($binaries.Glue, $binaries.Shim)) {
        if (-not (Test-Path -LiteralPath $path)) {
            throw "Build completed but $(Split-Path -Leaf $path) was not produced at $path"
        }
    }
    return $binaries
}

# The benchmark harness (`bench.exe`) lives in a separate project now, and nothing in the
# release graph depends on it, so a normal build cannot put the harness into
# glue.exe. This guards the one path that still could: -SkipBuild reuses
# whatever sits in the artifact directory, including a stale glue.exe built
# before the split. The markers are string literals in the harness, which
# survive the release profile's strip=true.
function Assert-NoBenchPayload {
    param([Parameter(Mandatory)][string]$Path)

    $text = [System.Text.Encoding]::ASCII.GetString([System.IO.File]::ReadAllBytes($Path))
    foreach ($marker in @('bench-fixtures', 'GLUE_BENCH_DEBUG')) {
        if ($text.IndexOf($marker, [System.StringComparison]::Ordinal) -ge 0) {
            throw ("$Path contains the development bench harness ('$marker'). " +
                'That code belongs to bench.exe. Rebuild glue.exe from the release tree.')
        }
    }
}

if (-not $Version) { $Version = Get-WorkspaceVersion }
if ($Version -notmatch '^\d+\.\d+\.\d+([-.+][0-9A-Za-z.-]+)?$') {
    throw "Invalid installer version '$Version'. Use a SemVer-like value such as 0.1.0."
}

$makensis = Find-Makensis
$outputDir = Join-Path $installerDir 'output'
$null = New-Item -ItemType Directory -Force -Path $outputDir

foreach ($targetArch in $Arch) {
    Write-Host "`n=== $targetArch MSVC ===" -ForegroundColor Cyan
    $binaries = Invoke-MsvcBuild -TargetArch $targetArch
    Assert-NoBenchPayload -Path $binaries.Glue
    $payloadDir = Join-Path (Join-Path $installerDir 'payload') $targetArch
    $null = New-Item -ItemType Directory -Force -Path $payloadDir
    Copy-Item -LiteralPath $binaries.Glue -Destination (Join-Path $payloadDir 'glue.exe') -Force
    Copy-Item -LiteralPath $binaries.Shim -Destination (Join-Path $payloadDir 'shim.exe') -Force

    Push-Location $installerDir
    try {
        Invoke-Native -FilePath $makensis -Arguments @(
            "/DPAYLOAD_VERSION=$Version", "/DPAYLOAD_ARCH=$targetArch", 'Glue.nsi'
        )
    } finally {
        Pop-Location
    }

    $installer = Join-Path $outputDir "GluestickSetup-$targetArch.exe"
    if (-not (Test-Path -LiteralPath $installer)) {
        throw "NSIS completed but installer was not produced: $installer"
    }
    $hash = (Get-FileHash -LiteralPath $installer -Algorithm SHA256).Hash.ToLowerInvariant()
    Set-Content -LiteralPath "$installer.sha256" -Value $hash -Encoding ascii -NoNewline
    Write-Host "Built: $installer" -ForegroundColor Green
    Write-Host "SHA256: $hash" -ForegroundColor DarkGray
}