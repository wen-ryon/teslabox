param(
    [Parameter(Mandatory = $true)][string]$Version,
    [Parameter(Mandatory = $true)][string]$PackageUrl,
    [Parameter(Mandatory = $true)][string]$PrivateKey,
    [string]$NotesFile = "",
    [string]$OutputDirectory = "dist\update"
)

$ErrorActionPreference = "Stop"
$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$privateKeyPath = (Resolve-Path $PrivateKey).Path
if ($privateKeyPath.StartsWith($root + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw "The release private key must be stored outside the repository."
}
$dirty = git -C $root status --porcelain
if ($LASTEXITCODE -ne 0) {
    throw "Unable to read the Git working tree status."
}
if ($dirty) {
    throw "Commit or clean the working tree before creating a release."
}
$output = Join-Path $root $OutputDirectory
New-Item -ItemType Directory -Force $output | Out-Null
$commit = (git -C $root rev-parse --short=12 HEAD).Trim()
if ($LASTEXITCODE -ne 0) {
    throw "Unable to read the Git commit."
}
$binary = Join-Path $output "teslabox-linux-arm64"
$helper = Join-Path $output "teslabox-update-helper-linux-arm64"
$binaryTemporary = "$binary.tmp"
$helperTemporary = "$helper.tmp"
$manifest = Join-Path $output "latest.json"
$versionManifest = Join-Path $output "manifest-$Version.json"
$ldflags = "-s -w -X github.com/wen-ryon/teslabox/internal/version.Version=$Version -X github.com/wen-ryon/teslabox/internal/version.Commit=$commit"

$previousGOOS = $env:GOOS
$previousGOARCH = $env:GOARCH
$previousCGO = $env:CGO_ENABLED
try {
    $env:GOOS = "linux"
    $env:GOARCH = "arm64"
    $env:CGO_ENABLED = "0"
    Remove-Item -Force -ErrorAction SilentlyContinue $binaryTemporary, $helperTemporary
    go build -trimpath -ldflags $ldflags -o $binaryTemporary .\cmd\teslabox
    if ($LASTEXITCODE -ne 0) {
        throw "Failed to build the TeslaBox ARM64 binary."
    }
    go build -trimpath -ldflags "-s -w" -o $helperTemporary .\cmd\teslabox-update-helper
    if ($LASTEXITCODE -ne 0) {
        throw "Failed to build the update helper."
    }
    Move-Item -Force $binaryTemporary $binary
    Move-Item -Force $helperTemporary $helper
} finally {
    $env:GOOS = $previousGOOS
    $env:GOARCH = $previousGOARCH
    $env:CGO_ENABLED = $previousCGO
}

$arguments = @(
    "run", ".\cmd\teslabox-update-manifest",
    "-private-key", $privateKeyPath,
    "-package", $binary,
    "-package-url", $PackageUrl,
    "-version", $Version,
    "-output", $manifest
)
if ($NotesFile) {
    $arguments += @("-notes-file", $NotesFile)
}
& go @arguments
if ($LASTEXITCODE -ne 0) {
    throw "Failed to generate the signed update manifest."
}
Copy-Item -Force $manifest $versionManifest

$binaryHash = (Get-FileHash -Algorithm SHA256 $binary).Hash.ToLowerInvariant()
$helperHash = (Get-FileHash -Algorithm SHA256 $helper).Hash.ToLowerInvariant()
$binaryChecksum = Join-Path $output "teslabox-linux-arm64.sha256"
$helperChecksum = Join-Path $output "teslabox-update-helper-linux-arm64.sha256"
# 发布校验文件固定使用 LF；Windows PowerShell 的 Set-Content 默认会写入 CRLF。
$binaryChecksumContent = "$binaryHash  teslabox-linux-arm64`n"
$helperChecksumContent = "$helperHash  teslabox-update-helper-linux-arm64`n"
[IO.File]::WriteAllText($binaryChecksum, $binaryChecksumContent, [Text.Encoding]::ASCII)
[IO.File]::WriteAllText($helperChecksum, $helperChecksumContent, [Text.Encoding]::ASCII)
foreach ($expected in @(
    @{ Path = $binaryChecksum; Content = $binaryChecksumContent },
    @{ Path = $helperChecksum; Content = $helperChecksumContent }
)) {
    $checksum = $expected.Path
    if (-not (Test-Path -LiteralPath $checksum -PathType Leaf) -or
        [IO.File]::ReadAllText($checksum, [Text.Encoding]::ASCII) -cne $expected.Content) {
        throw "Failed to create or verify update checksum file: $checksum"
    }
}

Get-FileHash -Algorithm SHA256 $binary, $helper, $manifest, $versionManifest, $binaryChecksum, $helperChecksum |
    Format-Table Path, Hash
