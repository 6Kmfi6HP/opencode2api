#!/usr/bin/env pwsh
<#
One-click installer for opencode2api on Windows.

Usage from PowerShell:

  irm https://raw.githubusercontent.com/6Kmfi6HP/opencode2api/main/scripts/install.ps1 | iex

Options:
  -Version <tag>           Release tag, for example v0.5.0
  -Repo <owner/repo>       GitHub repository to install from
  -InstallDir <path>       Binary install directory
  -NoModifyPath            Do not add the install directory to the user PATH
  -CheckOnly               Print detected arch/download URL without installing
  -Help                    Show this help
#>

[CmdletBinding()]
param(
    [string]$Version = $env:OPENCODE2API_VERSION,
    [string]$Repo = $(if (Test-Path Env:OPENCODE2API_REPO) { $env:OPENCODE2API_REPO } else { '6Kmfi6HP/opencode2api' }),
    [string]$InstallDir = $(if (Test-Path Env:OPENCODE2API_INSTALL_DIR) { $env:OPENCODE2API_INSTALL_DIR } else { Join-Path $HOME '.opencode2api\bin' }),
    [switch]$NoModifyPath,
    [switch]$CheckOnly,
    [switch]$Help
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

function Write-Info($Message) {
    Write-Host "[opencode2api] $Message" -ForegroundColor Cyan
}
function Write-Warn($Message) {
    Write-Host "[opencode2api] $Message" -ForegroundColor Yellow
}
function Write-Die($Message) {
    Write-Host "[opencode2api] $Message" -ForegroundColor Red
    exit 1
}

function Show-Usage {
    Get-Help $MyInvocation.MyCommand.Path -Full | Out-String | ForEach-Object { $_.TrimEnd() }
}

if ($Help) {
    Show-Usage
    exit 0
}

if ([string]::IsNullOrWhiteSpace($Repo)) {
    Write-Die 'missing repository'
}
if ([string]::IsNullOrWhiteSpace($InstallDir)) {
    Write-Die 'missing install directory'
}

# ---------------------------------------------------------------- arch ------

$ProcessorArch = $env:PROCESSOR_ARCHITECTURE
if ([string]::IsNullOrWhiteSpace($ProcessorArch)) {
    Write-Die 'PROCESSOR_ARCHITECTURE is not set; cannot detect Windows architecture'
}
switch -Regex ($ProcessorArch.ToUpperInvariant()) {
    '^(AMD64|X64)$' { $Arch = 'amd64' }
    '^ARM64$' { $Arch = 'arm64' }
    default {
        Write-Die "unsupported or unknown architecture: $ProcessorArch (try setting PROCESSOR_ARCHITECTURE=AMD64 or ARM64)"
    }
}

if ($CheckOnly) {
    Write-Host "os=windows"
    Write-Host "arch=$ProcessorArch"
    Write-Host "target=windows/$Arch"
    if ([string]::IsNullOrWhiteSpace($Version)) {
        Write-Host 'version=latest (will be resolved from GitHub API)'
        Write-Host "release_api=https://api.github.com/repos/$Repo/releases/latest"
    } else {
        $AssetName = "opencode2api_${Version}_windows_${Arch}.tar.gz"
        Write-Host "asset=$AssetName"
        Write-Host "download=https://github.com/$Repo/releases/download/$Version/$AssetName"
        Write-Host "checksums=https://github.com/$Repo/releases/download/$Version/checksums.txt"
    }
    exit 0
}

if ([string]::IsNullOrWhiteSpace($Version)) {
    Write-Info "resolving latest release from $Repo"
    try {
        $Headers = @{ 'User-Agent' = 'opencode2api-installer' }
        $Release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -Headers $Headers
        $Version = $Release.tag_name
    } catch {
        Write-Die "could not resolve latest release: $($_.Exception.Message)"
    }
    if ([string]::IsNullOrWhiteSpace($Version)) {
        Write-Die 'could not resolve latest release; use -Version <tag>'
    }
}

$AssetName = "opencode2api_${Version}_windows_${Arch}.tar.gz"
$BaseUrl = "https://github.com/$Repo/releases/download/$Version"

$TmpDir = Join-Path ([System.IO.Path]::GetTempPath()) ("opencode2api-install-" + [System.Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $TmpDir | Out-Null
try {
    $ArchivePath = Join-Path $TmpDir $AssetName
    $ChecksumsPath = Join-Path $TmpDir 'checksums.txt'

    Write-Info "downloading $BaseUrl/$AssetName"
    Invoke-WebRequest -UseBasicParsing -Uri "$BaseUrl/$AssetName" -OutFile $ArchivePath

    Write-Info "downloading $BaseUrl/checksums.txt"
    Invoke-WebRequest -UseBasicParsing -Uri "$BaseUrl/checksums.txt" -OutFile $ChecksumsPath

    $ExpectedLine = Select-String -Path $ChecksumsPath -Pattern ([regex]::Escape($AssetName)) | Select-Object -First 1
    if ($null -eq $ExpectedLine) {
        Write-Die "checksums.txt does not contain $AssetName"
    }
    $Parts = -split $ExpectedLine.Line.Trim()
    if ($Parts.Count -lt 1) {
        Write-Die "invalid checksums.txt line for $AssetName"
    }
    $Expected = $Parts[0]
    $Actual = (Get-FileHash -Path $ArchivePath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($Actual -ne $Expected.ToLowerInvariant()) {
        Write-Die "checksum mismatch for $AssetName"
    }
    Write-Info 'checksum verified'

    tar -xzf $ArchivePath -C $TmpDir
    $SrcDir = Join-Path $TmpDir "opencode2api_${Version}_windows_${Arch}"
    $SrcBin = Join-Path $SrcDir 'opencode2api.exe'
    if (-not (Test-Path -LiteralPath $SrcBin)) {
        Write-Die 'release archive did not contain opencode2api.exe'
    }

    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    $DestBin = Join-Path $InstallDir 'opencode2api.exe'
    Copy-Item -LiteralPath $SrcBin -Destination $DestBin -Force

    if (-not $NoModifyPath) {
        $UserPath = [Environment]::GetEnvironmentVariable('Path', 'User')
        $PathEntries = @()
        if (-not [string]::IsNullOrWhiteSpace($UserPath)) {
            $PathEntries = $UserPath -split ';' | Where-Object { -not [string]::IsNullOrWhiteSpace($_) }
        }
        if ($PathEntries -notcontains $InstallDir) {
            $NewPath = ($PathEntries + $InstallDir) -join ';'
            [Environment]::SetEnvironmentVariable('Path', $NewPath, 'User')
            Write-Info "PATH updated for the current user: $InstallDir"
        } else {
            Write-Info "PATH already contains: $InstallDir"
        }
    }

    Write-Info "installed opencode2api $Version to $DestBin"
    if (-not $NoModifyPath) {
        Write-Warn 'start a new PowerShell window so PATH changes take effect.'
    }

    Write-Host ''
    Write-Host 'Next steps:'
    Write-Host ''
    Write-Host '  opencode2api launch claude'
    Write-Host '  opencode2api launch codex'
} finally {
    Remove-Item -Recurse -Force -LiteralPath $TmpDir -ErrorAction SilentlyContinue
}
