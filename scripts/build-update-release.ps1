param(
    [Parameter(Mandatory = $true)][string]$Version,
    [Parameter(Mandatory = $true)][string]$PackageUrl,
    [Parameter(Mandatory = $true)][string]$PrivateKey,
    [ValidateSet("stable", "beta")][string]$Channel = "stable",
    [string]$NotesFile = "",
    [string]$MediaArchive = "build\media-toolchain\teslabox-media-debian11-arm64.tar.gz",
    [string]$OutputDirectory = "dist\update",
    [string]$CertificateEndpoint = "",
    [string]$CertificatePublicKey = "",
    [string]$CarPlayPublicAddress = "",
    [string]$ExpectedCommit = "",
    [string]$ExpectedMediaSHA256 = "",
    [string]$CarPlayArchive = "",
    [string]$CarPlaySHA256 = "",
    [switch]$AllowDirty
)

$ErrorActionPreference = "Stop"
if ($Version -notmatch '^\d+\.\d+\.\d+(?:-rc\.\d+)?$') {
    throw "Version must be a release version or an rc.N prerelease."
}
if ($Version -match '-rc\.' -and $Channel -ne "beta") {
    throw "RC releases must use the beta channel."
}
if ($CarPlayArchive -or $CarPlaySHA256) {
    if (-not $CarPlayArchive -or $CarPlaySHA256 -cnotmatch '^[0-9a-f]{64}$' -or
        $ExpectedCommit -cnotmatch '^[0-9a-f]{40}$' -or $ExpectedMediaSHA256 -cnotmatch '^[0-9a-f]{64}$' -or
        -not $CertificateEndpoint -or -not $CertificatePublicKey -or -not $CarPlayPublicAddress -or $AllowDirty) {
        throw "CarPlay builds require fixed input hashes, the full commit, product certificate settings and a clean tree."
    }
}
if ($CertificateEndpoint -or $CertificatePublicKey -or $CarPlayPublicAddress) {
    $certificateUri = $null
    $publicAddress = $null
    if (-not [Uri]::TryCreate($CertificateEndpoint, [UriKind]::Absolute, [ref]$certificateUri) -or
        $certificateUri.Scheme -ne "https" -or $certificateUri.UserInfo -or
        $certificateUri.Query -or $certificateUri.Fragment -or $certificateUri.AbsolutePath -ne "/" -or
        $CertificateEndpoint -match '\s' -or $CertificateEndpoint.Contains('"') -or $CertificateEndpoint.Contains("'")) {
        throw "CertificateEndpoint must be a plain HTTPS origin."
    }
    try { $certificateKeyBytes = [Convert]::FromBase64String($CertificatePublicKey) } catch { throw "Invalid certificate public key." }
    if ($certificateKeyBytes.Length -ne 32 -or -not [Net.IPAddress]::TryParse($CarPlayPublicAddress, [ref]$publicAddress) -or
        $publicAddress.AddressFamily -ne [Net.Sockets.AddressFamily]::InterNetwork) {
        throw "Certificate distribution needs an Ed25519 public key and an IPv4 routing alias."
    }
    $addressBytes = $publicAddress.GetAddressBytes()
    if ($addressBytes[0] -in @(0, 10, 127) -or $addressBytes[0] -ge 224 -or
        ($addressBytes[0] -eq 172 -and $addressBytes[1] -ge 16 -and $addressBytes[1] -le 31) -or
        ($addressBytes[0] -eq 192 -and $addressBytes[1] -eq 168) -or
        ($addressBytes[0] -eq 169 -and $addressBytes[1] -eq 254) -or
        $certificateUri.Host -eq "carplay.teslabox.online") {
        throw "The certificate service must use a separate hostname and a public IPv4 routing alias."
    }
}
$root = [IO.Path]::GetFullPath([IO.Path]::Combine($PSScriptRoot, ".."))
$fullCommit = (git -C $root rev-parse HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or $fullCommit -cnotmatch '^[0-9a-f]{40}$' -or
    ($ExpectedCommit -and $fullCommit -cne $ExpectedCommit)) {
    throw "The source HEAD does not match the fixed release commit."
}
$privateKeyPath = (Resolve-Path $PrivateKey).Path
if ($privateKeyPath.StartsWith($root + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw "The release private key must be stored outside the repository."
}
if (-not $AllowDirty) {
    $dirty = git -C $root status --porcelain
    if ($LASTEXITCODE -ne 0) {
        throw "Unable to read the Git working tree status."
    }
    if ($dirty) {
        throw "Commit or clean the working tree before creating a release."
    }
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
foreach ($artifact in @($binary, $helper, $binaryTemporary, $helperTemporary, $manifest, $versionManifest,
    (Join-Path $output "teslabox-linux-arm64.sha256"), (Join-Path $output "teslabox-update-helper-linux-arm64.sha256"))) {
    if (Test-Path -LiteralPath $artifact) {
        throw "Refusing to overwrite a release artifact: $artifact"
    }
}
$mediaPath = (Resolve-Path (Join-Path $root $MediaArchive)).Path
$mediaChecksumPath = "$mediaPath.sha256"
if (-not (Test-Path -LiteralPath $mediaChecksumPath -PathType Leaf)) {
    throw "The media archive checksum is missing: $mediaChecksumPath"
}
$mediaChecksum = ((Get-Content -Raw -Encoding ASCII -LiteralPath $mediaChecksumPath).Trim() -split '\s+')[0].ToLowerInvariant()
if ($mediaChecksum -ne (Get-FileHash -Algorithm SHA256 -LiteralPath $mediaPath).Hash.ToLowerInvariant()) {
    throw "The media archive checksum does not match: $mediaPath"
}
$bundleDirectory = Join-Path $root "internal\mediabundle\bundle"
if ($ExpectedMediaSHA256 -and $mediaChecksum -cne $ExpectedMediaSHA256) {
    throw "The media archive differs from the approved release input."
}
$carPlayPath = ""
if ($CarPlayArchive) {
    $carPlayPath = (Resolve-Path -LiteralPath $CarPlayArchive).Path
    if ((Get-FileHash -Algorithm SHA256 -LiteralPath $carPlayPath).Hash.ToLowerInvariant() -cne $CarPlaySHA256) {
        throw "The CarPlay runtime differs from the fixed release input."
    }
    $sidecar = [IO.File]::ReadAllText("$carPlayPath.sha256", [Text.Encoding]::ASCII).Trim()
    if ($sidecar -cne $CarPlaySHA256) {
        throw "The CarPlay runtime sidecar must contain only its approved SHA-256."
    }
    $packageText = & tar -xOf $carPlayPath PACKAGE.json
    if ($LASTEXITCODE -ne 0) { throw "The CarPlay runtime package metadata is missing." }
    $innerPackage = ($packageText -join "`n") | ConvertFrom-Json
    if ($innerPackage.schema -ne 1 -or $innerPackage.package_kind -cne "inner" -or
        $innerPackage.version -cne $Version -or $innerPackage.release_commit -cne $fullCommit) {
        throw "The CarPlay inner package is not bound to this release version and source commit."
    }
}
$carPlayBundleDirectory = Join-Path $root "internal\carplaybundle\bundle"
foreach ($directory in @($bundleDirectory, $carPlayBundleDirectory)) {
    if (Test-Path -LiteralPath $directory) {
        throw "Refusing to overwrite an existing embed directory: $directory"
    }
}
$buildTags = "releasebundle"
if ($carPlayPath) { $buildTags += ",carplaybundle" }
$ldflags = "-s -w -X github.com/wen-ryon/teslabox/internal/version.Version=$Version -X github.com/wen-ryon/teslabox/internal/version.Commit=$commit"

$previousGOOS = $env:GOOS
$previousGOARCH = $env:GOARCH
$previousCGO = $env:CGO_ENABLED
$previousGOCACHE = $env:GOCACHE
try {
	New-Item -ItemType Directory -Force -Path $bundleDirectory | Out-Null
	Copy-Item -Force -LiteralPath $mediaPath -Destination (Join-Path $bundleDirectory "teslabox-media-debian11-arm64.tar.gz")
	Copy-Item -Force -LiteralPath $mediaChecksumPath -Destination (Join-Path $bundleDirectory "teslabox-media-debian11-arm64.tar.gz.sha256")
    if ($carPlayPath) {
        New-Item -ItemType Directory -Path $carPlayBundleDirectory | Out-Null
        Copy-Item -LiteralPath $carPlayPath -Destination (Join-Path $carPlayBundleDirectory "carplay-runtime.tar.gz")
        Copy-Item -LiteralPath "$carPlayPath.sha256" -Destination (Join-Path $carPlayBundleDirectory "carplay-runtime.tar.gz.sha256")
    }
    $env:GOOS = "linux"
    $env:GOARCH = "arm64"
    $env:CGO_ENABLED = "0"
    $env:GOCACHE = Join-Path $root ".gocache"
    if ($CertificateEndpoint) {
        $ldflags += " -X github.com/wen-ryon/teslabox/internal/certdelivery.DefaultEndpoint=$CertificateEndpoint"
        $ldflags += " -X github.com/wen-ryon/teslabox/internal/certdelivery.DefaultPublicKey=$CertificatePublicKey"
        $ldflags += " -X github.com/wen-ryon/teslabox/internal/certdelivery.DefaultPublicAddress=$CarPlayPublicAddress"
    }
    go -C $root build -trimpath -tags $buildTags -ldflags $ldflags -o $binaryTemporary .\cmd\teslabox
    if ($LASTEXITCODE -ne 0) {
        throw "Failed to build the TeslaBox ARM64 binary."
    }
    if ((Get-Item -LiteralPath $binaryTemporary).Length -gt 134217728) {
        throw "The main ELF exceeds the 128 MiB limit of existing update helpers."
    }
    go -C $root build -trimpath -ldflags "-s -w" -o $helperTemporary .\cmd\teslabox-update-helper
    if ($LASTEXITCODE -ne 0) {
        throw "Failed to build the update helper."
    }
    Move-Item -LiteralPath $binaryTemporary -Destination $binary
    Move-Item -LiteralPath $helperTemporary -Destination $helper
} finally {
    foreach ($embed in @(
        (Join-Path $bundleDirectory "teslabox-media-debian11-arm64.tar.gz"),
        (Join-Path $bundleDirectory "teslabox-media-debian11-arm64.tar.gz.sha256"),
        (Join-Path $carPlayBundleDirectory "carplay-runtime.tar.gz"),
        (Join-Path $carPlayBundleDirectory "carplay-runtime.tar.gz.sha256")
    )) { Remove-Item -LiteralPath $embed -Force -ErrorAction SilentlyContinue }
    foreach ($directory in @($bundleDirectory, $carPlayBundleDirectory)) {
        Remove-Item -LiteralPath $directory -ErrorAction SilentlyContinue
    }
    $env:GOOS = $previousGOOS
    $env:GOARCH = $previousGOARCH
    $env:CGO_ENABLED = $previousCGO
    $env:GOCACHE = $previousGOCACHE
}

$arguments = @(
    "run", ".\cmd\teslabox-update-manifest",
    "-private-key", $privateKeyPath,
    "-package", $binary,
    "-package-url", $PackageUrl,
    "-version", $Version,
    "-channel", $Channel,
    "-output", $manifest
)
if ($NotesFile) {
    $arguments += @("-notes-file", $NotesFile)
}
$manifestGOCACHE = $env:GOCACHE
$manifestGOOS = $env:GOOS
$manifestGOARCH = $env:GOARCH
$manifestCGO = $env:CGO_ENABLED
try {
    # 发布包必须交叉编译为 Linux/ARM64；签名工具则必须使用当前 Windows 宿主架构执行。
    # 不依赖调用终端遗留的 GOOS/GOARCH，避免 go run 生成无法在本机启动的交叉编译程序。
    $env:GOOS = (go env GOHOSTOS).Trim()
    $env:GOARCH = (go env GOHOSTARCH).Trim()
    $env:CGO_ENABLED = "1"
    $env:GOCACHE = Join-Path $root ".gocache"
    & go -C $root @arguments
    if ($LASTEXITCODE -ne 0) {
        throw "Failed to generate the signed update manifest."
    }
} finally {
    $env:GOCACHE = $manifestGOCACHE
    $env:GOOS = $manifestGOOS
    $env:GOARCH = $manifestGOARCH
    $env:CGO_ENABLED = $manifestCGO
}
Copy-Item -Force $manifest $versionManifest

$binaryHash = (Get-FileHash -Algorithm SHA256 $binary).Hash.ToLowerInvariant()
$helperHash = (Get-FileHash -Algorithm SHA256 $helper).Hash.ToLowerInvariant()
$binaryChecksum = Join-Path $output "teslabox-linux-arm64.sha256"
$helperChecksum = Join-Path $output "teslabox-update-helper-linux-arm64.sha256"
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
