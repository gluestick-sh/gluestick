#Requires -Version 5.1
<#
.SYNOPSIS
  Remove Glue PATH entries during uninstall.
#>
param(
    [Parameter(Mandatory = $true)]
    [string]$GlueRoot
)

$ErrorActionPreference = 'Stop'

function Remove-DirFromUserPath {
    param([string]$Dir)
    $Dir = $Dir.TrimEnd('\').ToLowerInvariant()
    $current = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not $current) { return }

    $parts = $current -split ';' | Where-Object {
        $_.Trim().TrimEnd('\').ToLowerInvariant() -ne $Dir -and $_.Trim() -ne ''
    }
    $newPath = ($parts -join ';').Trim(';')
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
}

$root = $GlueRoot.TrimEnd('\')
Remove-DirFromUserPath -Dir $root
Remove-DirFromUserPath -Dir (Join-Path $root 'shims')

Write-Host "Removed Glue PATH entries for $root"
