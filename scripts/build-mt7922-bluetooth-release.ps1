param(
    [string]$BaseArchive = "dist\bootstrap-inputs\teslabox-mt7922-bluetooth-rock3a-5.10.160teslabox2-public.tar.gz",
    [string]$FirmwareDirectory = "dist\bootstrap-inputs\mt7922-firmware-20260916",
    [string]$OutputDirectory = "dist\bootstrap"
)

$ErrorActionPreference = "Stop"
$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$baseArchivePath = Join-Path $root $BaseArchive
$baseArchiveHash = "482a41aadad95fa8bf3b3df06e42ea2e374c5788442d7be39449076500638fd5"
if (-not (Test-Path -LiteralPath $baseArchivePath -PathType Leaf)) {
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $baseArchivePath) | Out-Null
    & curl.exe --fail --location --retry 3 --connect-timeout 15 --max-time 180 --output $baseArchivePath `
        "https://gitee.com/TecheN1/teslabox-releases/releases/download/v0.1.23/teslabox-mt7922-bluetooth-rock3a-5.10.160teslabox2.tar.gz"
    if ($LASTEXITCODE -ne 0) {
        throw "Failed to download the validated teslabox2 archive."
    }
}
if ((Get-FileHash -Algorithm SHA256 -LiteralPath $baseArchivePath).Hash.ToLowerInvariant() -ne $baseArchiveHash) {
    throw "Unexpected teslabox2 base archive checksum: $baseArchivePath"
}
$firmwareDirectoryPath = Join-Path $root $FirmwareDirectory
$output = Join-Path $root $OutputDirectory
$work = Join-Path $output "mt7922-bluetooth-stage"
$package = Join-Path $work "package"
$wifiFirmware = @{
    "WIFI_MT7922_patch_mcu_1_1_hdr.bin" = "7fc9075d6d54b31e0539734614ca971fa52caf41b642a3000c8a4501591a4d98"
    "WIFI_RAM_CODE_MT7922_1.bin" = "d100e5e04c324fc8da6302112d1e0b4fac952286447d2419eabcd8fb0a1f5b93"
}
New-Item -ItemType Directory -Force -Path $firmwareDirectoryPath | Out-Null
foreach ($name in $wifiFirmware.Keys) {
    $path = Join-Path $firmwareDirectoryPath $name
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        $urls = @(
            "https://git.kernel.org/pub/scm/linux/kernel/git/firmware/linux-firmware.git/plain/mediatek/${name}?h=20260916",
            "https://gitlab.com/kernel-firmware/linux-firmware/-/raw/20260916/mediatek/$name"
        )
        foreach ($url in $urls) {
            & curl.exe --fail --location --retry 3 --connect-timeout 15 --max-time 120 --output $path $url
            if ($LASTEXITCODE -eq 0 -and (Test-Path -LiteralPath $path -PathType Leaf) -and
                (Get-FileHash -Algorithm SHA256 -LiteralPath $path).Hash.ToLowerInvariant() -eq $wifiFirmware[$name]) {
                break
            }
        }
    }
    if (-not (Test-Path -LiteralPath $path -PathType Leaf) -or
        (Get-Item -LiteralPath $path).Length -eq 0 -or
        (Get-FileHash -Algorithm SHA256 -LiteralPath $path).Hash.ToLowerInvariant() -ne $wifiFirmware[$name]) {
        throw "Missing or invalid MT7922 firmware: $path (linux-firmware 20260916)."
    }
}
$licenseName = "LICENCE.mediatek"
$licenseSha256 = "a90d3f66704d85889945fec5525ea77622549da83aced1aac99828383f8f1805"
$licensePath = Join-Path $firmwareDirectoryPath $licenseName
if (-not (Test-Path -LiteralPath $licensePath -PathType Leaf)) {
    & curl.exe --fail --location --retry 3 --connect-timeout 15 --max-time 120 --output $licensePath `
        "https://gitlab.com/kernel-firmware/linux-firmware/-/raw/20260916/LICENSES/$licenseName"
    if ($LASTEXITCODE -ne 0) {
        throw "Failed to download the MediaTek firmware license."
    }
}
if ((Get-FileHash -Algorithm SHA256 -LiteralPath $licensePath).Hash.ToLowerInvariant() -ne $licenseSha256) {
    throw "Invalid MediaTek firmware license: $licensePath"
}

if (Test-Path -LiteralPath $work) {
    $resolvedWork = (Resolve-Path -LiteralPath $work).Path
    if (-not $resolvedWork.StartsWith($root + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
        throw "MT7922 build stage is outside the repository: $resolvedWork"
    }
    Remove-Item -LiteralPath $work -Recurse -Force
}
New-Item -ItemType Directory -Force -Path $package | Out-Null
tar -C $package -xzf $baseArchivePath
if ($LASTEXITCODE -ne 0) {
    throw "Failed to extract the validated teslabox2 archive."
}
$requiredFiles = @(
    "linux-image-5.10.160teslabox2_5.10.160teslabox2-1_arm64.deb",
    "bluetooth\btusb.ko",
    "bluetooth\btrtl.ko",
    "bluetooth\BT_RAM_CODE_MT7922_1_1_hdr.bin",
    "wifi-updates\drivers\net\wireless\mediatek\mt76\mt7921\mt7921e.ko"
)
foreach ($relative in $requiredFiles) {
    $requiredPath = Join-Path $package $relative
    if (-not (Test-Path -LiteralPath $requiredPath -PathType Leaf) -or
        (Get-Item -LiteralPath $requiredPath).Length -eq 0) {
        throw "The validated teslabox2 archive is missing: $relative"
    }
}
New-Item -ItemType Directory -Force -Path (Join-Path $package "firmware\mediatek") | Out-Null
foreach ($name in $wifiFirmware.Keys) {
    Copy-Item -LiteralPath (Join-Path $firmwareDirectoryPath $name) -Destination (Join-Path $package "firmware\mediatek\$name")
}
Copy-Item -LiteralPath $licensePath -Destination (Join-Path $package "firmware\LICENCE.mediatek")

$installScript = @'
#!/bin/sh
set -eu

BASE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
KERNEL=5.10.160teslabox2

if [ "$(id -u)" -ne 0 ]; then
    echo "Run this MT7922 installer with sudo." >&2
    exit 1
fi
if [ "$(uname -m)" != "aarch64" ]; then
    echo "MT7922 Bluetooth drivers require ARM64." >&2
    exit 1
fi

cd "${BASE}"
sha256sum -c SHA256SUMS
dpkg -i linux-image-5.10.160teslabox2_5.10.160teslabox2-1_arm64.deb

install -d -m 0755 "/lib/modules/${KERNEL}/updates" "/lib/modules/${KERNEL}/updates/teslabox-bluetooth"
cp -a wifi-updates/. "/lib/modules/${KERNEL}/updates/"
install -m 0644 bluetooth/btusb.ko "/lib/modules/${KERNEL}/updates/teslabox-bluetooth/btusb.ko"
install -m 0644 bluetooth/btrtl.ko "/lib/modules/${KERNEL}/updates/teslabox-bluetooth/btrtl.ko"
install -d -m 0755 /lib/firmware/mediatek
install -m 0644 bluetooth/BT_RAM_CODE_MT7922_1_1_hdr.bin /lib/firmware/mediatek/BT_RAM_CODE_MT7922_1_1_hdr.bin
install -m 0644 firmware/mediatek/WIFI_MT7922_patch_mcu_1_1_hdr.bin /lib/firmware/mediatek/WIFI_MT7922_patch_mcu_1_1_hdr.bin
install -m 0644 firmware/mediatek/WIFI_RAM_CODE_MT7922_1.bin /lib/firmware/mediatek/WIFI_RAM_CODE_MT7922_1.bin
install -d -m 0755 /usr/share/doc/teslabox
install -m 0644 firmware/LICENCE.mediatek /usr/share/doc/teslabox/LICENCE.mediatek
depmod -a "${KERNEL}"
update-initramfs -u -k "${KERNEL}"

for required in btusb btrtl mt7921e; do
    if ! modinfo -k "${KERNEL}" "${required}" >/dev/null 2>&1; then
        echo "Missing kernel module after installation: ${required}" >&2
        exit 1
    fi
done
if [ ! -s /lib/firmware/mediatek/BT_RAM_CODE_MT7922_1_1_hdr.bin ]; then
    echo "Missing MT7922 Bluetooth firmware after installation." >&2
    exit 1
fi
if [ ! -s /lib/firmware/mediatek/WIFI_MT7922_patch_mcu_1_1_hdr.bin ] ||
   [ ! -s /lib/firmware/mediatek/WIFI_RAM_CODE_MT7922_1.bin ]; then
    echo "Missing MT7922 Wi-Fi firmware after installation." >&2
    exit 1
fi
echo "MT7922 Bluetooth drivers installed; reboot to use 5.10.160teslabox2."
'@
[IO.File]::WriteAllText((Join-Path $package "install.sh"), ($installScript -replace "`r`n", "`n"), (New-Object Text.UTF8Encoding($false)))
$installerBytes = [IO.File]::ReadAllBytes((Join-Path $package "install.sh"))
if ($installerBytes -contains 0x0D -or @($installerBytes | Where-Object { $_ -gt 0x7F }).Count -gt 0) {
    throw "The generated MT7922 installer must be ASCII and use LF line endings."
}

$checksumLines = Get-ChildItem -LiteralPath $package -Recurse -File |
    Where-Object Name -ne "SHA256SUMS" |
    Sort-Object FullName |
    ForEach-Object {
        $relative = $_.FullName.Substring($package.Length + 1).Replace("\", "/")
        $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $_.FullName).Hash.ToLowerInvariant()
        "$hash  $relative"
    }
[IO.File]::WriteAllText((Join-Path $package "SHA256SUMS"), (($checksumLines -join "`n") + "`n"), [Text.Encoding]::ASCII)

$archive = Join-Path $output "teslabox-mt7922-bluetooth-rock3a-5.10.160teslabox2-fw20260916.tar.gz"
if (Test-Path -LiteralPath $archive) {
    Remove-Item -LiteralPath $archive -Force
}
tar -C $package -czf $archive .
if ($LASTEXITCODE -ne 0) {
    throw "Failed to create the MT7922 Bluetooth release archive."
}
$hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $archive).Hash.ToLowerInvariant()
[IO.File]::WriteAllText("$archive.sha256", "$hash  $(Split-Path -Leaf $archive)`n", [Text.Encoding]::ASCII)

[PSCustomObject]@{ Archive = $archive; Size = (Get-Item $archive).Length; SHA256 = $hash }
