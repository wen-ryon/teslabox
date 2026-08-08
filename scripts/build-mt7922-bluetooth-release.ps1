param(
    [string]$TestArchive = "build\mt7922\teslabox-mt7922-bluetooth-rock3a-5.10.160teslabox2-test4.tar.gz",
    [string]$BTUSBModule = "build\mt7922\btusb-5.10.160teslabox2-mt7922-79xx-v3.ko",
    [string]$BTRTLModule = "build\mt7922\btrtl-5.10.160teslabox2-mt7922-79xx.ko",
    [string]$OutputDirectory = "dist\bootstrap"
)

$ErrorActionPreference = "Stop"
$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$testArchivePath = (Resolve-Path (Join-Path $root $TestArchive)).Path
$btusbPath = (Resolve-Path (Join-Path $root $BTUSBModule)).Path
$btrtlPath = (Resolve-Path (Join-Path $root $BTRTLModule)).Path
$output = Join-Path $root $OutputDirectory
$work = Join-Path $output "mt7922-bluetooth-stage"
$source = Join-Path $work "source"
$package = Join-Path $work "package"

if (Test-Path -LiteralPath $work) {
    Remove-Item -LiteralPath $work -Recurse -Force
}
New-Item -ItemType Directory -Force -Path $source, $package | Out-Null
tar -C $source -xzf $testArchivePath
if ($LASTEXITCODE -ne 0) {
    throw "Failed to extract the validated MT7922 Bluetooth test archive."
}

$kernelDeb = Get-ChildItem -LiteralPath $source -Recurse -Filter "linux-image-5.10.160teslabox2_*_arm64.deb" | Select-Object -First 1
$backportsDeb = Get-ChildItem -LiteralPath $source -Recurse -Filter "linux-mt7922-backports-5.10.160teslabox2_*_arm64.deb" | Select-Object -First 1
if (-not $kernelDeb -or -not $backportsDeb) {
    throw "The test archive does not contain the teslabox2 kernel and backports packages."
}
Copy-Item -LiteralPath $kernelDeb.FullName -Destination (Join-Path $package $kernelDeb.Name)

$debStage = Join-Path $work "deb"
New-Item -ItemType Directory -Path $debStage | Out-Null
Push-Location $debStage
try {
    ar x $backportsDeb.FullName
    if ($LASTEXITCODE -ne 0) {
        throw "Failed to unpack the backports Debian archive."
    }
    7z x -y data.tar.xz "-o$debStage\data-layer" | Out-Null
    7z x -y (Join-Path $debStage "data-layer\data.tar") "-o$debStage\root" | Out-Null
} finally {
    Pop-Location
}

$nestedModules = Join-Path $debStage "root\lib\modules\5.10.160teslabox2\lib\modules\5.10.160teslabox2\updates"
$firmware = Join-Path $debStage "root\lib\firmware\mediatek\BT_RAM_CODE_MT7922_1_1_hdr.bin"
if (-not (Test-Path -LiteralPath $nestedModules -PathType Container) -or
    -not (Test-Path -LiteralPath $firmware -PathType Leaf)) {
    throw "The validated package is missing teslabox2 modules or MT7922 Bluetooth firmware."
}
Copy-Item -LiteralPath $nestedModules -Destination (Join-Path $package "wifi-updates") -Recurse
New-Item -ItemType Directory -Force -Path (Join-Path $package "bluetooth") | Out-Null
Copy-Item -LiteralPath $btusbPath -Destination (Join-Path $package "bluetooth\btusb.ko")
Copy-Item -LiteralPath $btrtlPath -Destination (Join-Path $package "bluetooth\btrtl.ko")
Copy-Item -LiteralPath $firmware -Destination (Join-Path $package "bluetooth\BT_RAM_CODE_MT7922_1_1_hdr.bin")

$installScript = @'
#!/bin/sh
set -eu

BASE=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
KERNEL=5.10.160teslabox2

if [ "$(id -u)" -ne 0 ]; then
    echo "请使用 sudo 运行 MT7922 蓝牙安装程序。" >&2
    exit 1
fi
if [ "$(uname -m)" != "aarch64" ]; then
    echo "MT7922 蓝牙驱动只支持 ARM64。" >&2
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
depmod -a "${KERNEL}"
update-initramfs -u -k "${KERNEL}"

for required in btusb btrtl mt7921e; do
    if ! modinfo -k "${KERNEL}" "${required}" >/dev/null 2>&1; then
        echo "安装后缺少内核模块：${required}" >&2
        exit 1
    fi
done
if [ ! -s /lib/firmware/mediatek/BT_RAM_CODE_MT7922_1_1_hdr.bin ]; then
    echo "安装后缺少 MT7922 蓝牙固件。" >&2
    exit 1
fi
echo "MT7922 蓝牙驱动已安装；重启后使用 5.10.160teslabox2 生效。"
'@
[IO.File]::WriteAllText((Join-Path $package "install.sh"), ($installScript -replace "`r`n", "`n"), (New-Object Text.UTF8Encoding($false)))

$checksumLines = Get-ChildItem -LiteralPath $package -Recurse -File |
    Where-Object Name -ne "SHA256SUMS" |
    Sort-Object FullName |
    ForEach-Object {
        $relative = $_.FullName.Substring($package.Length + 1).Replace("\", "/")
        $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $_.FullName).Hash.ToLowerInvariant()
        "$hash  $relative"
    }
[IO.File]::WriteAllText((Join-Path $package "SHA256SUMS"), (($checksumLines -join "`n") + "`n"), [Text.Encoding]::ASCII)

$archive = Join-Path $output "teslabox-mt7922-bluetooth-rock3a-5.10.160teslabox2.tar.gz"
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
