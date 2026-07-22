param(
    [Parameter(Mandatory = $true)][string]$KernelDeb,
    [Parameter(Mandatory = $true)][string]$BackportsDeb,
    [string]$OutputDirectory = "dist\bootstrap"
)

$ErrorActionPreference = "Stop"
$root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$kernelPath = (Resolve-Path $KernelDeb).Path
$backportsPath = (Resolve-Path $BackportsDeb).Path

$expectedKernel = "linux-image-5.10.160teslabox1_5.10.160teslabox1-2_arm64.deb"
$expectedBackports = "linux-mt7922-backports-5.10.160teslabox1_5.10.160teslabox1-2_arm64.deb"
if ((Split-Path -Leaf $kernelPath) -ne $expectedKernel) {
    throw "Unexpected kernel package name: $kernelPath"
}
if ((Split-Path -Leaf $backportsPath) -ne $expectedBackports) {
    throw "Unexpected backports package name: $backportsPath"
}

$output = Join-Path $root $OutputDirectory
$stage = Join-Path $output "mt7922-stage"
if (Test-Path -LiteralPath $stage) {
    Remove-Item -LiteralPath $stage -Recurse -Force
}
New-Item -ItemType Directory -Force -Path $stage | Out-Null
Copy-Item -LiteralPath $kernelPath -Destination (Join-Path $stage $expectedKernel)
Copy-Item -LiteralPath $backportsPath -Destination (Join-Path $stage $expectedBackports)

$checksumLines = Get-ChildItem -LiteralPath $stage -File | Sort-Object Name | ForEach-Object {
    $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $_.FullName).Hash.ToLowerInvariant()
    "$hash  $($_.Name)"
}
if (-not $checksumLines) {
    throw "No packages were found while creating SHA256SUMS."
}
$checksumPath = Join-Path $stage "SHA256SUMS"
# 显式逐行追加 LF，避免 Windows PowerShell 将 CRLF 混入 Linux 文件名。
Set-Content -Encoding Ascii -NoNewline -LiteralPath $checksumPath -Value ""
foreach ($checksumLine in $checksumLines) {
    $null = [IO.File]::AppendAllText($checksumPath, $checksumLine + [char]10, [Text.Encoding]::ASCII)
}

$archive = Join-Path $output "teslabox-mt7922-rock3a-5.10.160teslabox1.tar.gz"
if (Test-Path -LiteralPath $archive) {
    Remove-Item -LiteralPath $archive -Force
}
tar -C $stage -czf $archive .
if ($LASTEXITCODE -ne 0) {
    throw "Failed to create the MT7922 archive."
}

$archiveHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $archive).Hash.ToLowerInvariant()
$archiveChecksum = "$archiveHash  $(Split-Path -Leaf $archive)`n"
[IO.File]::WriteAllText("$archive.sha256", $archiveChecksum, [Text.Encoding]::ASCII)

[PSCustomObject]@{
    Archive = $archive
    Size = (Get-Item -LiteralPath $archive).Length
    SHA256 = $archiveHash
}
