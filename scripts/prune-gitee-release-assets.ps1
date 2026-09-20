[CmdletBinding(SupportsShouldProcess = $true, ConfirmImpact = "High")]
param(
    [string]$Owner = "TecheN1",
    [string]$Repository = "teslabox-releases",
    [ValidateRange(1, 20)][int]$Keep = 5,
    [string]$AccessToken = $env:GITEE_ACCESS_TOKEN,
    [string]$HistoryPath = "dist\teslabox-releases\stable\history.json",
    [string]$BetaHistoryPath = "dist\teslabox-releases\beta\history.json",
    [switch]$Apply
)

$ErrorActionPreference = "Stop"

function Get-GiteeAPIUri {
    param([Parameter(Mandatory = $true)][string]$Path)

    return "https://gitee.com/api/v5$Path"
}

function Invoke-GiteeAPI {
    param(
        [Parameter(Mandatory = $true)][ValidateSet("Delete", "Get")][string]$Method,
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][hashtable]$Headers
    )

    $parameters = @{ Method = $Method; Uri = Get-GiteeAPIUri -Path $Path; Headers = $Headers }
    if ($Method -eq "Get") {
        return Invoke-RestMethod @parameters
    }
    Invoke-WebRequest @parameters | Out-Null
}

function ConvertTo-ManagedRelease {
    param([Parameter(Mandatory = $true)][string]$Tag)

    if ($Tag -notmatch '^v(\d+)\.(\d+)\.(\d+)(-beta)?$') {
        return $null
    }
    return [PSCustomObject]@{
        Version = [version]::new([int]$Matches[1], [int]$Matches[2], [int]$Matches[3])
        ManifestVersion = "$($Matches[1]).$($Matches[2]).$($Matches[3])"
        Channel = if ($Matches[4]) { "beta" } else { "stable" }
    }
}

if ($Owner -notmatch '^[A-Za-z0-9_.-]+$' -or $Repository -notmatch '^[A-Za-z0-9_.-]+$') {
    throw "Gitee 所有者或仓库名包含不支持的字符。"
}
if ([string]::IsNullOrWhiteSpace($AccessToken)) {
    throw "请通过 GITEE_ACCESS_TOKEN 环境变量或 -AccessToken 提供具有项目权限的 Gitee Token。"
}

$headers = @{ Authorization = "Bearer $AccessToken"; Accept = "application/json" }
$releasePath = "/repos/$Owner/$Repository/releases"
$releases = @()
for ($page = 1; ; $page++) {
    $pageItems = @(Invoke-GiteeAPI -Method Get -Path "$releasePath?page=$page&per_page=100" -Headers $headers)
    $releases += $pageItems
    if ($pageItems.Count -lt 100) {
        break
    }
}

$managedReleases = @(
    foreach ($release in $releases) {
        $managed = ConvertTo-ManagedRelease -Tag ([string]$release.tag_name)
        if ($null -ne $managed) {
            [PSCustomObject]@{ Release = $release; Version = $managed.Version; ManifestVersion = $managed.ManifestVersion; Channel = $managed.Channel }
        }
    }
)
$retired = @(
    foreach ($channel in @("stable", "beta")) {
        @($managedReleases | Where-Object { $_.Channel -eq $channel } | Sort-Object Version -Descending | Select-Object -Skip $Keep)
    }
)
if ($retired.Count -eq 0) {
    Write-Output "稳定版和内测版都未超过 $Keep 个可清理 Release。"
    exit 0
}

$root = Split-Path -Parent $PSScriptRoot
$protectedVersions = @{}
foreach ($channel in @($retired | ForEach-Object { $_.Channel } | Select-Object -Unique)) {
    $historyPath = if ($channel -eq "beta") { $BetaHistoryPath } else { $HistoryPath }
    $historyFile = Join-Path $root $historyPath
    if (-not (Test-Path -LiteralPath $historyFile -PathType Leaf)) {
        throw "找不到 $channel 通道历史目录：$historyFile"
    }
    $history = Get-Content -Raw -Encoding UTF8 -LiteralPath $historyFile | ConvertFrom-Json
    $protectedVersions[$channel] = @($history.manifests | ForEach-Object { [string]$_.version })
}
$blocked = @($retired | Where-Object { $protectedVersions[$_.Channel] -contains $_.ManifestVersion })
if ($blocked.Count -gt 0) {
    $versions = ($blocked | ForEach-Object { $_.Release.tag_name }) -join ", "
    throw "拒绝删除附件：对应签名 history.json 仍提供这些版本：$versions。先用最近 $Keep 个固定 manifest 重新签名并推送相应通道目录，再执行本脚本。"
}

foreach ($item in $retired) {
    $release = $item.Release
    $attachments = @(Invoke-GiteeAPI -Method Get -Path "$releasePath/$($release.id)/attach_files" -Headers $headers)
    if ($attachments.Count -eq 0) {
        Write-Output "$($release.tag_name)：没有可清理附件。"
        continue
    }
    foreach ($attachment in $attachments) {
        $description = "$($release.tag_name)/$($attachment.name)"
        if (-not $Apply) {
            Write-Output "预览：将删除 $description"
            continue
        }
        if ($PSCmdlet.ShouldProcess($description, "删除 Gitee Release 附件")) {
            Invoke-GiteeAPI -Method Delete -Path "$releasePath/$($release.id)/attach_files/$($attachment.id)" -Headers $headers
            Write-Output "已删除：$description"
        }
    }
}
