param(
    [Parameter(Mandatory = $true)][string]$Version,
    [ValidateSet("stable", "beta")][string]$Channel = "beta",
    [Parameter(Mandatory = $true)][string]$BootstrapArchive,
    [Parameter(Mandatory = $true)][string]$MT7922Archive,
    [Parameter(Mandatory = $true)][string]$OutputFile
)

$ErrorActionPreference = "Stop"
$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$bootstrapPath = (Resolve-Path (Join-Path $root $BootstrapArchive)).Path
$mt7922Path = (Resolve-Path (Join-Path $root $MT7922Archive)).Path
$outputPath = [IO.Path]::GetFullPath((Join-Path $root $OutputFile))
$distRoot = [IO.Path]::GetFullPath((Join-Path $root "dist"))
if (-not $outputPath.StartsWith($distRoot + [IO.Path]::DirectorySeparatorChar, [StringComparison]::OrdinalIgnoreCase)) {
    throw "The installer output must be inside dist."
}
if ($Version -notmatch '^\d+\.\d+\.\d+(?:-rc\.[1-9]\d*)?$') {
    throw "Invalid release version: $Version"
}
if ($Channel -eq "stable" -and $Version.Contains("-rc.")) {
    throw "Stable installers cannot use an RC version."
}

$tag = "v$Version"
if ($Channel -eq "beta" -and -not $Version.Contains("-rc.")) {
    $tag += "-beta"
}
$bootstrapName = Split-Path -Leaf $bootstrapPath
$mt7922Name = Split-Path -Leaf $mt7922Path
if ($bootstrapName -ne "teslabox-bootstrap-debian11-arm64.tar.gz" -or
    $mt7922Name -ne "teslabox-mt7922-bluetooth-rock3a-5.10.160teslabox2-fw20260916.tar.gz") {
    throw "Unexpected bootstrap or MT7922 archive name."
}

$sourcePath = Join-Path $root "deploy\install.sh"
$sourceBytes = [IO.File]::ReadAllBytes($sourcePath)
if ($sourceBytes.Length -ge 3 -and $sourceBytes[0] -eq 0xEF -and $sourceBytes[1] -eq 0xBB -and $sourceBytes[2] -eq 0xBF) {
    throw "The installer source contains a UTF-8 BOM."
}
if ($sourceBytes -contains 0x0D) {
    throw "The installer source must use LF line endings."
}
$utf8Strict = New-Object System.Text.UTF8Encoding($false, $true)
$lines = $utf8Strict.GetString($sourceBytes).TrimEnd("`n") -split "`n"
$values = @{
    INSTALLER_VERSION = if ($Channel -eq "beta" -and -not $Version.Contains("-rc.")) { "$Version-beta" } else { $Version }
    INSTALL_CHANNEL = $Channel
    RELEASE_BASE = "https://gitee.com/TecheN1/teslabox-releases/releases/download/$tag"
    BOOTSTRAP_ARCHIVE = $bootstrapName
    BOOTSTRAP_SHA256 = (Get-FileHash -Algorithm SHA256 -LiteralPath $bootstrapPath).Hash.ToLowerInvariant()
    MT7922_BLUETOOTH_RELEASE_BASE = "https://gitee.com/TecheN1/teslabox-releases/releases/download/$tag"
    MT7922_BLUETOOTH_ARCHIVE = $mt7922Name
    MT7922_BLUETOOTH_SHA256 = (Get-FileHash -Algorithm SHA256 -LiteralPath $mt7922Path).Hash.ToLowerInvariant()
}
foreach ($key in $values.Keys) {
    $prefix = "$key="
    $matching = @($lines | Where-Object { $_.StartsWith($prefix, [StringComparison]::Ordinal) })
    if ($matching.Count -ne 1) {
        throw "Expected exactly one installer setting: $key"
    }
    for ($i = 0; $i -lt $lines.Length; $i++) {
        if ($lines[$i].StartsWith($prefix, [StringComparison]::Ordinal)) {
            $lines[$i] = '{0}="{1}"' -f $key, $values[$key]
            break
        }
    }
}

New-Item -ItemType Directory -Force -Path (Split-Path -Parent $outputPath) | Out-Null
[IO.File]::WriteAllText($outputPath, (($lines -join "`n") + "`n"), (New-Object System.Text.UTF8Encoding($false)))
[PSCustomObject]@{
    Installer = $outputPath
    Version = $Version
    Channel = $Channel
    SHA256 = (Get-FileHash -Algorithm SHA256 -LiteralPath $outputPath).Hash.ToLowerInvariant()
}
