<#
.SYNOPSIS
  ais Windows installer.

.DESCRIPTION
  Downloads the latest ais release for Windows, verifies its SHA256 checksum,
  installs ais.exe, and adds the install directory to your user PATH.

  Usage:
    irm https://raw.githubusercontent.com/sqmch/ais/main/scripts/install.ps1 | iex

  Optional environment variables:
    AIS_REPO         GitHub repo in "owner/name" format (default: sqmch/ais)
    AIS_VERSION      Release tag (e.g. v0.2.0). If unset, uses the latest release.
    AIS_INSTALL_DIR  Installation directory (default: %LOCALAPPDATA%\Programs\ais)
    AIS_BIN_NAME     Installed binary name without extension (default: ais)
#>

$ErrorActionPreference = 'Stop'

function Write-Info { param([string]$Message) Write-Host $Message }
function Fail { param([string]$Message) Write-Error "Error: $Message"; exit 1 }

function Get-AisArch {
    switch ($env:PROCESSOR_ARCHITECTURE) {
        'AMD64' { return 'amd64' }
        'ARM64' { return 'arm64' }
        'x86' {
            # 32-bit PowerShell on a 64-bit OS reports x86; trust the OS instead.
            if ([Environment]::Is64BitOperatingSystem) { return 'amd64' }
            Fail 'Unsupported architecture: x86. Supported: amd64, arm64.'
        }
        default {
            if ([Environment]::Is64BitOperatingSystem) { return 'amd64' }
            Fail "Unsupported architecture: $($env:PROCESSOR_ARCHITECTURE). Supported: amd64, arm64."
        }
    }
}

$repo = if ($env:AIS_REPO) { $env:AIS_REPO } else { 'sqmch/ais' }
$version = if ($env:AIS_VERSION) { $env:AIS_VERSION } else { 'latest' }
$binName = if ($env:AIS_BIN_NAME) { $env:AIS_BIN_NAME } else { 'ais' }
$installDir = if ($env:AIS_INSTALL_DIR) { $env:AIS_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\ais' }

$arch = Get-AisArch
$artifact = "ais_windows_$arch.zip"
$checksums = 'checksums.txt'

if ($version -eq 'latest') {
    $baseUrl = "https://github.com/$repo/releases/latest/download"
} else {
    $baseUrl = "https://github.com/$repo/releases/download/$version"
}

$workdir = Join-Path ([System.IO.Path]::GetTempPath()) ("ais-install-" + [System.Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $workdir -Force | Out-Null
try {
    $artifactPath = Join-Path $workdir $artifact
    $checksumsPath = Join-Path $workdir $checksums

    Write-Info "Downloading $artifact from $baseUrl"
    try {
        Invoke-WebRequest -Uri "$baseUrl/$artifact" -OutFile $artifactPath -UseBasicParsing
    } catch {
        Fail "Failed to download $artifact."
    }
    try {
        Invoke-WebRequest -Uri "$baseUrl/$checksums" -OutFile $checksumsPath -UseBasicParsing
    } catch {
        Fail "Failed to download checksums.txt."
    }

    $expectedLine = Get-Content $checksumsPath |
        Where-Object { $_ -match "\s$([regex]::Escape($artifact))$" } |
        Select-Object -First 1
    if (-not $expectedLine) { Fail "Checksum entry for $artifact not found." }
    $expected = (($expectedLine -split '\s+')[0]).ToLowerInvariant()
    $actual = (Get-FileHash -Path $artifactPath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($expected -ne $actual) { Fail "Checksum mismatch for $artifact." }
    Write-Info 'Checksum verified.'

    $extractDir = Join-Path $workdir 'extract'
    New-Item -ItemType Directory -Path $extractDir -Force | Out-Null
    Expand-Archive -Path $artifactPath -DestinationPath $extractDir -Force
    $extractedBin = Join-Path $extractDir 'ais.exe'
    if (-not (Test-Path $extractedBin)) { Fail "Archive did not contain expected binary 'ais.exe'." }

    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    $target = Join-Path $installDir "$binName.exe"
    Copy-Item -Path $extractedBin -Destination $target -Force
    Write-Info "Installed $binName to $target"

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $onPath = ($userPath -split ';') -contains $installDir
    if (-not $onPath) {
        $newPath = if ([string]::IsNullOrEmpty($userPath)) { $installDir } else { "$userPath;$installDir" }
        [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
        $env:Path = "$env:Path;$installDir"
        Write-Info "Added $installDir to your user PATH. Open a new terminal to pick it up."
    }

    Write-Info "Run: $binName --help"
}
finally {
    Remove-Item -Path $workdir -Recurse -Force -ErrorAction SilentlyContinue
}
