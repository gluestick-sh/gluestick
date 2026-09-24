#Requires -Version 5.1
<#
.SYNOPSIS
  Post-install steps for GlueSetup (NSIS): unblock, MinGit, PATH, glue path setup.
#>
param(
    [Parameter(Mandatory = $true)]
    [string]$GlueRoot,

    [Parameter(Mandatory = $true)]
    [ValidateSet('amd64', 'arm64')]
    [string]$Arch
)

$ErrorActionPreference = 'Stop'

function Add-DirToUserPath {
    param([string]$Dir)
    $Dir = $Dir.TrimEnd('\')
    $current = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not $current) { $current = '' }
    $parts = @()
    foreach ($part in $current -split ';') {
        $trim = $part.Trim()
        if (-not $trim) { continue }
        if ($trim.TrimEnd('\') -ieq $Dir) { continue }
        $parts += $trim
    }
    $newPath = if ($parts.Count -gt 0) { "$Dir;$($parts -join ';')" } else { $Dir }
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
}

function Disable-PythonAppAliases {
    $apps = Join-Path $env:LOCALAPPDATA 'Microsoft\WindowsApps'
    foreach ($name in @('python.exe', 'python3.exe', 'pythonw.exe')) {
        $p = Join-Path $apps $name
        if (-not (Test-Path -LiteralPath $p)) { continue }
        $item = Get-Item -LiteralPath $p -Force -ErrorAction SilentlyContinue
        if ($item -and -not $item.PSIsContainer -and $item.Length -le 1024) {
            Remove-Item -LiteralPath $p -Force -ErrorAction SilentlyContinue
        }
    }
}

function Install-MinGit {
    param(
        [string]$Root,
        [string]$CpuArch
    )

    $gitVerify = if ($CpuArch -eq 'arm64') {
        'clangarm64\bin\git.exe'
    } else {
        'mingw64\bin\git.exe'
    }

    $binDir = Join-Path $Root 'bin'
    $zipPath = Join-Path $binDir 'mingit.zip'
    $gitDir = Join-Path $binDir 'git'
    $verifyPath = Join-Path $gitDir $gitVerify

    if (Test-Path -LiteralPath $verifyPath) {
        Write-Host "MinGit already present: $verifyPath"
        return
    }
    if (-not (Test-Path -LiteralPath $zipPath)) {
        throw "Missing $zipPath"
    }

    Write-Host "Extracting MinGit -> $gitDir"
    if (Test-Path -LiteralPath $gitDir) {
        Remove-Item -LiteralPath $gitDir -Recurse -Force
    }
    $null = New-Item -ItemType Directory -Force -Path $gitDir
    Expand-Archive -LiteralPath $zipPath -DestinationPath $gitDir -Force

    if (-not (Test-Path -LiteralPath $verifyPath)) {
        throw "MinGit layout unexpected (missing $verifyPath)"
    }
}

$glueExe = Join-Path $GlueRoot 'glue.exe'
$shimExe = Join-Path $GlueRoot 'shim.exe'
$sevenZ = Join-Path $GlueRoot 'bin\7z.exe'
$sevenDll = Join-Path $GlueRoot 'bin\7z.dll'

foreach ($path in @($glueExe, $shimExe, $sevenZ, $sevenDll)) {
    if (-not (Test-Path -LiteralPath $path)) {
        throw "Install incomplete, missing: $path"
    }
}

Get-ChildItem -LiteralPath $GlueRoot -Recurse -Include *.exe, *.dll -File -ErrorAction SilentlyContinue |
    ForEach-Object { Unblock-File -LiteralPath $_.FullName -ErrorAction SilentlyContinue }

Install-MinGit -Root $GlueRoot -CpuArch $Arch
Add-DirToUserPath -Dir $GlueRoot
Add-DirToUserPath -Dir (Join-Path $GlueRoot 'shims')
Disable-PythonAppAliases

& $glueExe path setup
if ($LASTEXITCODE -ne 0) {
    throw "glue path setup failed with exit code $LASTEXITCODE"
}

Write-Host "Glue installed to $GlueRoot"
