param(
    [Parameter(Mandatory = $true)][string]$Version,
    [string]$MainBinary = "dist\update\teslabox-linux-arm64",
    [string]$UpdateHelper = "dist\update\teslabox-update-helper-linux-arm64",
    [string]$MediaArchive = "build\media-toolchain\teslabox-media-debian11-arm64.tar.gz",
    [string]$OutputDirectory = "dist\bootstrap",
    [switch]$AllowDirty
)

$ErrorActionPreference = "Stop"
$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path

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
$mediaChecksumPath = "$mediaPath.sha256"
if (-not (Test-Path -LiteralPath $mediaChecksumPath -PathType Leaf)) {
    throw "The media archive checksum is missing: $mediaChecksumPath"
}

$manifestPath = Join-Path (Split-Path -Parent $mainPath) "latest.json"
if (Test-Path -LiteralPath $manifestPath -PathType Leaf) {
    $manifest = Get-Content -Raw -Encoding UTF8 -LiteralPath $manifestPath | ConvertFrom-Json
    $mainHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $mainPath).Hash.ToLowerInvariant()
    $mainSize = (Get-Item -LiteralPath $mainPath).Length
    if ($manifest.version -ne $Version -or $manifest.sha256 -ne $mainHash -or [int64]$manifest.size -ne $mainSize) {
        throw "The main binary does not match its signed latest.json manifest."
    }
}

$output = Join-Path $root $OutputDirectory
$stage = Join-Path $output "stage"
$resolvedOutputParent = (Resolve-Path (Split-Path -Parent $output)).Path
if (-not $output.StartsWith($resolvedOutputParent + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw "The output directory must stay inside the repository."
}
if (Test-Path -LiteralPath $stage) {
    Remove-Item -LiteralPath $stage -Recurse -Force
}

$directories = @(
    "bin", "libexec", "config", "systemd", "udev", "scripts", "media"
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
try {
    $env:GOOS = "linux"
    $env:GOARCH = "arm64"
    $env:CGO_ENABLED = "0"
    $commands = @(
        @{ Package = ".\cmd\teslabox-init"; Output = "bin\teslabox-init" },
        @{ Package = ".\cmd\teslabox-image-init"; Output = "bin\teslabox-image-init" },
        @{ Package = ".\cmd\teslabox-gadget"; Output = "bin\teslabox-gadget" }
    )
    foreach ($command in $commands) {
        $target = Join-Path $stage $command.Output
        go build -trimpath -ldflags "-s -w" -o $target $command.Package
        if ($LASTEXITCODE -ne 0) {
            throw "Failed to build $($command.Package)."
        }
    }
} finally {
    $env:GOOS = $previousGOOS
    $env:GOARCH = $previousGOARCH
    $env:CGO_ENABLED = $previousCGO
}

$files = @{
    "configs\teslabox.env.example" = "config\teslabox.env.example"
    "deploy\systemd\teslabox.service" = "systemd\teslabox.service"
    "deploy\systemd\teslabox-update.service" = "systemd\teslabox-update.service"
    "deploy\systemd\teslabox-ap.service" = "systemd\teslabox-ap.service"
    "deploy\udev\99-teslabox.rules" = "udev\99-teslabox.rules"
    "deploy\scripts\teslabox-ap-setup.sh" = "scripts\teslabox-ap-setup.sh"
    "deploy\media-toolchain\install.sh" = "scripts\install-media-toolchain.sh"
}
foreach ($entry in $files.GetEnumerator()) {
    Copy-Item -LiteralPath (Join-Path $root $entry.Key) -Destination (Join-Path $stage $entry.Value)
}
Copy-Item -LiteralPath $mediaPath -Destination (Join-Path $stage "media\teslabox-media-debian11-arm64.tar.gz")
Copy-Item -LiteralPath $mediaChecksumPath -Destination (Join-Path $stage "media\teslabox-media-debian11-arm64.tar.gz.sha256")

# Windows 工作区可能启用 CRLF；发布包内的 Linux 配置必须统一为 LF 且不带 BOM。
$textFiles = Get-ChildItem -LiteralPath $stage -Recurse -File
foreach ($file in $textFiles) {
    if ($file.Extension -notin @(".sh", ".service", ".rules") -and -not $file.Name.EndsWith(".env.example")) {
        continue
    }
    $content = [IO.File]::ReadAllText($file.FullName).Replace("`r`n", "`n")
    [IO.File]::WriteAllBytes($file.FullName, ([Text.Encoding]::UTF8).GetBytes($content))
}

$info = @(
    "version=$Version",
    "commit=$commit",
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
Set-Content -Encoding Ascii -LiteralPath (Join-Path $stage "SHA256SUMS") -Value $checksumLines

$archive = Join-Path $output "teslabox-bootstrap-debian11-arm64.tar.gz"
if (Test-Path -LiteralPath $archive) {
    Remove-Item -LiteralPath $archive -Force
}
tar -C $stage -czf $archive .
if ($LASTEXITCODE -ne 0) {
    throw "Failed to create the bootstrap archive."
}

$archiveHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $archive).Hash.ToLowerInvariant()
Set-Content -Encoding Ascii -LiteralPath "$archive.sha256" -Value "$archiveHash  $(Split-Path -Leaf $archive)"

[PSCustomObject]@{
    Version = $Version
    Commit = $commit
    Archive = $archive
    Size = (Get-Item -LiteralPath $archive).Length
    SHA256 = $archiveHash
}
