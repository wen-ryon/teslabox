param(
    [Parameter(Mandatory = $true)][string]$Version,
    [string]$MainBinary = "dist\update\teslabox-linux-arm64",
    [string]$UpdateHelper = "dist\update\teslabox-update-helper-linux-arm64",
    [string]$MediaArchive = "build\media-toolchain\teslabox-media-debian11-arm64.tar.gz",
    [string]$RadxaKeyring = "dist\bootstrap-inputs\radxa-archive-keyring_0.2.2_all.deb",
    [string]$OutputDirectory = "dist\bootstrap",
    [string]$ExpectedCommit = "",
    [string]$ExpectedMainSHA256 = "",
    [string]$ExpectedHelperSHA256 = "",
    [string]$ExpectedMediaSHA256 = "",
    [switch]$AllowDirty
)

$ErrorActionPreference = "Stop"
$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
if ($Version -notmatch '^\d+\.\d+\.\d+(?:-rc\.[1-9]\d*)?$') {
    throw "Invalid release version."
}
$fixedInputs = $ExpectedCommit -or $ExpectedMainSHA256 -or $ExpectedHelperSHA256 -or $ExpectedMediaSHA256 -or $Version -eq "0.2.0-rc.1"
if ($fixedInputs -and ($ExpectedCommit -cnotmatch '^[0-9a-f]{40}$' -or
    $ExpectedMainSHA256 -cnotmatch '^[0-9a-f]{64}$' -or $ExpectedHelperSHA256 -cnotmatch '^[0-9a-f]{64}$' -or
    $ExpectedMediaSHA256 -cnotmatch '^[0-9a-f]{64}$' -or $AllowDirty)) {
    throw "Bootstrap requires the fixed commit, main/helper/media hashes and a clean source tree."
}
$fullCommit = (git -C $root rev-parse HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or ($ExpectedCommit -and $fullCommit -cne $ExpectedCommit)) {
    throw "Bootstrap source HEAD differs from the fixed release commit."
}

function Assert-LinuxTextFile {
    param([Parameter(Mandatory = $true)][string]$Path)

    $bytes = [IO.File]::ReadAllBytes($Path)
    if ($bytes.Length -eq 0) {
        throw "Required Linux source text file is empty: $Path"
    }
    if ($bytes.Length -ge 3 -and $bytes[0] -eq 0xEF -and $bytes[1] -eq 0xBB -and $bytes[2] -eq 0xBF) {
        throw "Linux source text file must not contain a UTF-8 BOM: $Path"
    }
    if ($bytes -contains 0x0D) {
        throw "Linux source text file must use LF line endings: $Path"
    }
    $strictUTF8 = New-Object -TypeName System.Text.UTF8Encoding -ArgumentList $false, $true
    try {
        [void]$strictUTF8.GetString($bytes)
    } catch {
        throw "Linux source text file is not valid UTF-8: $Path"
    }
}

if (-not $AllowDirty) {
    $dirty = git -C $root status --porcelain
    if ($LASTEXITCODE -ne 0) {
        throw "Unable to read the Git working tree status."
    }
    if ($dirty) {
        throw "Commit or clean the working tree before creating a bootstrap release."
    }
}

$mainPath = (Resolve-Path (Join-Path $root $MainBinary)).Path
$helperPath = (Resolve-Path (Join-Path $root $UpdateHelper)).Path
$mediaPath = (Resolve-Path (Join-Path $root $MediaArchive)).Path
$radxaKeyringPath = (Resolve-Path (Join-Path $root $RadxaKeyring)).Path
$mediaChecksumPath = "$mediaPath.sha256"
if (-not (Test-Path -LiteralPath $mediaChecksumPath -PathType Leaf)) {
    throw "The media archive checksum is missing: $mediaChecksumPath"
}
$mainHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $mainPath).Hash.ToLowerInvariant()
$helperHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $helperPath).Hash.ToLowerInvariant()
$mediaHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $mediaPath).Hash.ToLowerInvariant()
$mediaSidecar = ((Get-Content -Raw -Encoding ASCII -LiteralPath $mediaChecksumPath).Trim() -split '\s+')[0]
if ($mediaSidecar -cne $mediaHash -or ($ExpectedMediaSHA256 -and $mediaHash -cne $ExpectedMediaSHA256) -or
    ($ExpectedMainSHA256 -and $mainHash -cne $ExpectedMainSHA256) -or
    ($ExpectedHelperSHA256 -and $helperHash -cne $ExpectedHelperSHA256)) {
    throw "Bootstrap input digest does not match its fixed release input."
}
foreach ($executable in @($mainPath, $helperPath)) {
    $stream = [IO.File]::OpenRead($executable)
    try {
        $header = New-Object byte[] 64
        if ($stream.Read($header, 0, 64) -ne 64 -or
            $header[0] -ne 127 -or $header[1] -ne 69 -or $header[2] -ne 76 -or $header[3] -ne 70 -or
            $header[4] -ne 2 -or $header[5] -ne 1 -or $header[18] -ne 183 -or $header[19] -ne 0) {
            throw "Bootstrap executable is not a Linux ARM64 ELF: $executable"
        }
    } finally { $stream.Dispose() }
}
$radxaKeyringHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $radxaKeyringPath).Hash.ToLowerInvariant()
if ($radxaKeyringHash -ne "3e3bb3d3150cf1dfa6c050d0c135f6a7f298b7638ff7e656fda711ababdb8a57") {
    throw "The Radxa archive keyring checksum is unexpected: $radxaKeyringPath"
}

$manifestPath = Join-Path (Split-Path -Parent $mainPath) "latest.json"
if ($fixedInputs -and -not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) {
    throw "The fixed main executable must have its release manifest."
}
if (Test-Path -LiteralPath $manifestPath -PathType Leaf) {
    $manifest = Get-Content -Raw -Encoding UTF8 -LiteralPath $manifestPath | ConvertFrom-Json
    $mainHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $mainPath).Hash.ToLowerInvariant()
    $mainSize = (Get-Item -LiteralPath $mainPath).Length
    if ($manifest.version -ne $Version -or $manifest.sha256 -ne $mainHash -or [int64]$manifest.size -ne $mainSize) {
        throw "The main binary does not match its signed latest.json manifest."
    }
}

$output = [IO.Path]::GetFullPath((Join-Path $root $OutputDirectory))
$stage = Join-Path $output "stage"
$archive = Join-Path $output "teslabox-bootstrap-debian11-arm64.tar.gz"
if (-not $output.StartsWith($root + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw "The output directory must stay inside the repository."
}
foreach ($existing in @($stage, $archive, "$archive.sha256")) {
    if (Test-Path -LiteralPath $existing) {
        throw "Refusing to overwrite a bootstrap artifact: $existing"
    }
}

$directories = @(
    "bin", "libexec", "config", "systemd", "udev", "scripts", "media", "keyring"
)
foreach ($directory in $directories) {
    New-Item -ItemType Directory -Force -Path (Join-Path $stage $directory) | Out-Null
}

Copy-Item -LiteralPath $mainPath -Destination (Join-Path $stage "bin\teslabox")
Copy-Item -LiteralPath $helperPath -Destination (Join-Path $stage "libexec\teslabox-update-helper")

$commit = (git -C $root rev-parse --short=12 HEAD).Trim()
if ($LASTEXITCODE -ne 0) {
    throw "Unable to read the Git commit."
}

$previousGOOS = $env:GOOS
$previousGOARCH = $env:GOARCH
$previousCGO = $env:CGO_ENABLED
$previousGOCACHE = $env:GOCACHE
try {
    $env:GOOS = "linux"
    $env:GOARCH = "arm64"
    $env:CGO_ENABLED = "0"
    $env:GOCACHE = Join-Path $root ".gocache"
    $commands = @(
        @{ Package = ".\cmd\teslabox-init"; Output = "bin\teslabox-init" },
        @{ Package = ".\cmd\teslabox-image-init"; Output = "bin\teslabox-image-init" },
        @{ Package = ".\cmd\teslabox-gadget"; Output = "bin\teslabox-gadget" }
    )
    foreach ($command in $commands) {
        $target = Join-Path $stage $command.Output
        go -C $root build -trimpath -ldflags "-s -w" -o $target $command.Package
        if ($LASTEXITCODE -ne 0) {
            throw "Failed to build $($command.Package)."
        }
    }
} finally {
    $env:GOOS = $previousGOOS
    $env:GOARCH = $previousGOARCH
    $env:CGO_ENABLED = $previousCGO
    $env:GOCACHE = $previousGOCACHE
}

$files = @{
    "configs\teslabox.env.example" = "config\teslabox.env.example"
    "deploy\systemd\teslabox.service" = "systemd\teslabox.service"
    "deploy\systemd\teslabox-update.service" = "systemd\teslabox-update.service"
    "deploy\systemd\teslabox-ap.service" = "systemd\teslabox-ap.service"
    "deploy\systemd\teslabox-fan-early.service" = "systemd\teslabox-fan-early.service"
    "deploy\udev\99-teslabox.rules" = "udev\99-teslabox.rules"
    "deploy\scripts\teslabox-ap-setup.sh" = "scripts\teslabox-ap-setup.sh"
    "deploy\scripts\teslabox-fan-early.sh" = "scripts\teslabox-fan-early.sh"
    "deploy\media-toolchain\install.sh" = "scripts\install-media-toolchain.sh"
}
foreach ($entry in $files.GetEnumerator()) {
    $destination = Join-Path $stage $entry.Value
    $source = Join-Path $root $entry.Key
    Assert-LinuxTextFile -Path $source
    $sourceLength = (Get-Item -LiteralPath $source).Length
    Copy-Item -LiteralPath $source -Destination $destination
    $destinationLength = (Get-Item -LiteralPath $destination).Length
    if ($destinationLength -ne $sourceLength) {
        throw "Linux text file copy size mismatch: $destination (source bytes=$sourceLength, destination bytes=$destinationLength)"
    }
    if ((Get-FileHash -Algorithm SHA256 -LiteralPath $source).Hash -ne (Get-FileHash -Algorithm SHA256 -LiteralPath $destination).Hash) {
        throw "Linux text file copy checksum mismatch: $destination"
    }
}
Copy-Item -LiteralPath $mediaPath -Destination (Join-Path $stage "media\teslabox-media-debian11-arm64.tar.gz")
Copy-Item -LiteralPath $mediaChecksumPath -Destination (Join-Path $stage "media\teslabox-media-debian11-arm64.tar.gz.sha256")
Copy-Item -LiteralPath $radxaKeyringPath -Destination (Join-Path $stage "keyring\radxa-archive-keyring_0.2.2_all.deb")

$info = @(
    "version=$Version",
    "commit=$commit",
    "full_commit=$fullCommit",
    "main_sha256=$mainHash",
    "helper_sha256=$helperHash",
    "media_sha256=$mediaHash",
    "board=radxa-rock-3a",
    "os=debian-11",
    "arch=arm64"
) -join "`n"
[IO.File]::WriteAllBytes((Join-Path $stage "BUILD_INFO"), ([Text.Encoding]::UTF8).GetBytes($info + "`n"))

$checksumLines = Get-ChildItem -LiteralPath $stage -Recurse -File | Sort-Object FullName | ForEach-Object {
    $relative = $_.FullName.Substring($stage.Length + 1).Replace("\", "/")
    $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $_.FullName).Hash.ToLowerInvariant()
    "$hash  $relative"
}
if (-not $checksumLines) {
    throw "No files were found while creating SHA256SUMS."
}
$checksumPath = Join-Path $stage "SHA256SUMS"
# 显式逐行追加 LF，避免 Windows PowerShell 将 CRLF 混入 Linux 文件名。
Set-Content -Encoding Ascii -NoNewline -LiteralPath $checksumPath -Value ""
foreach ($checksumLine in $checksumLines) {
    $null = [IO.File]::AppendAllText($checksumPath, $checksumLine + [char]10, [Text.Encoding]::ASCII)
}

tar -C $stage -czf $archive .
if ($LASTEXITCODE -ne 0) {
    throw "Failed to create the bootstrap archive."
}

$archiveHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $archive).Hash.ToLowerInvariant()
$archiveChecksum = "$archiveHash  $(Split-Path -Leaf $archive)`n"
[IO.File]::WriteAllText("$archive.sha256", $archiveChecksum, [Text.Encoding]::ASCII)

[PSCustomObject]@{
    Version = $Version
    Commit = $commit
    Archive = $archive
    Size = (Get-Item -LiteralPath $archive).Length
    SHA256 = $archiveHash
}
