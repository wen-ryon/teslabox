$ErrorActionPreference = 'Stop'

$RepositoryRoot = Split-Path -Parent $PSScriptRoot
$BuildContext = Join-Path $RepositoryRoot 'deploy\media-toolchain'
$RemoteHost = if ($env:TESLABOX_BUILDER_HOST) { $env:TESLABOX_BUILDER_HOST } else { '192.0.2.58' }
$RemoteUser = if ($env:TESLABOX_BUILDER_USER) { $env:TESLABOX_BUILDER_USER } else { 'root' }
$RemoteRoot = '/root/teslabox-media-build'
$ImageName = 'localhost/teslabox-media-builder:debian11-arm64'

# 密码和密钥由调用者的 SSH 环境提供，脚本不会把凭据写入仓库或远端。
ssh "$RemoteUser@$RemoteHost" "mkdir -p '$RemoteRoot/context' '$RemoteRoot/work'"
scp (Join-Path $BuildContext 'Containerfile') "${RemoteUser}@${RemoteHost}:${RemoteRoot}/context/Containerfile"
scp (Join-Path $BuildContext 'build-inside.sh') "${RemoteUser}@${RemoteHost}:${RemoteRoot}/context/build-inside.sh"

ssh "$RemoteUser@$RemoteHost" "chmod 0755 '$RemoteRoot/context/build-inside.sh' && docker build -t '$ImageName' -f '$RemoteRoot/context/Containerfile' '$RemoteRoot/context' && docker run --rm -v '$RemoteRoot/work:/work' '$ImageName'"

$OutputDirectory = Join-Path $RepositoryRoot 'build\media-toolchain'
New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null
scp "${RemoteUser}@${RemoteHost}:${RemoteRoot}/work/output/teslabox-media-debian11-arm64.tar.gz*" $OutputDirectory

Write-Host "构建产物已下载到 $OutputDirectory"
