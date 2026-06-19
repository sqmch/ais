<#
.SYNOPSIS
  ais Windows uninstaller.

.DESCRIPTION
  Removes ais.exe and drops the install directory from your user PATH.

  Usage:
    irm https://raw.githubusercontent.com/sqmch/ais/main/scripts/uninstall.ps1 | iex

  Optional environment variables:
    AIS_INSTALL_DIR  Installation directory (default: %LOCALAPPDATA%\Programs\ais)
    AIS_BIN_NAME     Binary name without extension (default: ais)
#>

$ErrorActionPreference = 'Stop'

function Write-Info { param([string]$Message) Write-Host $Message }

$binName = if ($env:AIS_BIN_NAME) { $env:AIS_BIN_NAME } else { 'ais' }
$installDir = if ($env:AIS_INSTALL_DIR) { $env:AIS_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\ais' }
$target = Join-Path $installDir "$binName.exe"

if (Test-Path $target) {
    Remove-Item -Path $target -Force
    Write-Info "Removed $target"
} else {
    Write-Info "Nothing to remove at $target"
}

# Drop the install dir from the user PATH if present.
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($userPath) {
    $parts = $userPath -split ';' | Where-Object { $_ -and ($_ -ne $installDir) }
    $newPath = $parts -join ';'
    if ($newPath -ne $userPath) {
        [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
        Write-Info "Removed $installDir from your user PATH."
    }
}

# Remove the install directory if it is now empty.
if ((Test-Path $installDir) -and -not (Get-ChildItem -Path $installDir -Force)) {
    Remove-Item -Path $installDir -Force
}
