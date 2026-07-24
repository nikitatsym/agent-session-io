$ErrorActionPreference = "Stop"

$Repository = "nikitatsym/agent-session-io"
$Release = $env:SESSIONIO_VERSION
if ([string]::IsNullOrWhiteSpace($Release)) {
    $Release = "latest"
}

$InstallDir = $env:SESSIONIO_INSTALL_DIR
if ([string]::IsNullOrWhiteSpace($InstallDir)) {
    $InstallDir = Join-Path $env:LOCALAPPDATA "Programs\sessionio"
}

switch ($env:PROCESSOR_ARCHITECTURE.ToUpperInvariant()) {
    "AMD64" { $Architecture = "amd64" }
    "ARM64" { $Architecture = "arm64" }
    default { throw "sessionio installer: unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
}

$Archive = "sessionio_windows_${Architecture}.zip"
if ($Release -eq "latest") {
    $ReleaseUrl = "https://github.com/${Repository}/releases/latest/download"
} else {
    if (-not $Release.StartsWith("v")) {
        $Release = "v${Release}"
    }
    $ReleaseUrl = "https://github.com/${Repository}/releases/download/${Release}"
}

$TemporaryDir = Join-Path ([System.IO.Path]::GetTempPath()) ("sessionio-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $TemporaryDir | Out-Null

try {
    $ArchivePath = Join-Path $TemporaryDir $Archive
    $ChecksumsPath = Join-Path $TemporaryDir "checksums.txt"
    Invoke-WebRequest -UseBasicParsing "${ReleaseUrl}/${Archive}" -OutFile $ArchivePath
    Invoke-WebRequest -UseBasicParsing "${ReleaseUrl}/checksums.txt" -OutFile $ChecksumsPath

    $Pattern = "\s+$([regex]::Escape($Archive))$"
    $ChecksumLine = Get-Content $ChecksumsPath | Where-Object { $_ -match $Pattern } | Select-Object -First 1
    if ([string]::IsNullOrWhiteSpace($ChecksumLine)) {
        throw "sessionio installer: archive checksum is missing"
    }

    $ExpectedHash = ($ChecksumLine -split "\s+")[0].ToUpperInvariant()
    $ActualHash = (Get-FileHash -Algorithm SHA256 $ArchivePath).Hash.ToUpperInvariant()
    if ($ActualHash -ne $ExpectedHash) {
        throw "sessionio installer: archive checksum mismatch"
    }

    $ExtractDir = Join-Path $TemporaryDir "archive"
    Expand-Archive -Path $ArchivePath -DestinationPath $ExtractDir
    $Binary = Join-Path $ExtractDir "sessionio.exe"
    if (-not (Test-Path $Binary -PathType Leaf)) {
        throw "sessionio installer: archive does not contain sessionio.exe"
    }

    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    Copy-Item -Force $Binary (Join-Path $InstallDir "sessionio.exe")

    $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $PathEntries = @($UserPath -split ";" | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
    if ($InstallDir -notin $PathEntries) {
        $NewUserPath = (($PathEntries + $InstallDir) -join ";")
        [Environment]::SetEnvironmentVariable("Path", $NewUserPath, "User")
    }
    $env:Path = "${InstallDir};$env:Path"

    Write-Host "installed sessionio to $(Join-Path $InstallDir 'sessionio.exe')"
} finally {
    Remove-Item -Recurse -Force $TemporaryDir
}
