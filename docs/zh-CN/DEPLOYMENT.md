# TeslaBox 构建与部署

本文说明如何在 Windows 开发机上构建 Linux ARM64 二进制，并通过 SSH 更新 ROCK 3A。官方服务默认监听 HTTP `80` 端口，因此访问地址不再需要写 `:8080`。

## 前提

- Windows 已安装 Go 1.26.3 和 OpenSSH 客户端。
- ROCK 3A 可通过局域网访问，示例地址为 `192.0.2.38`，登录用户为 `rock`。
- 板端已经安装 `teslabox.service`，服务以 `root` 运行，因此可绑定 `80` 端口。
- 执行升级前确认外接数据盘和 USB OTG 当前状态，实车写入期间不要升级。

端口 `80` 若已被 nginx、Apache 等程序占用，TeslaBox 会启动失败。升级前可在板端检查：

```sh
sudo ss -ltnp '( sport = :80 )'
```

没有输出表示当前没有进程监听该端口。

## Windows 构建

在 PowerShell 中进入仓库：

```powershell
Set-Location D:\GoProject\src\teslabox
go test ./...
go vet ./...

$env:GOOS="linux"
$env:GOARCH="arm64"
$env:CGO_ENABLED="0"
go build -trimpath -o .\bin\teslabox-linux-arm64 .\cmd\teslabox
Remove-Item Env:GOOS,Env:GOARCH,Env:CGO_ENABLED

Get-FileHash -Algorithm SHA256 .\bin\teslabox-linux-arm64
```

构建产物为 `D:\GoProject\src\teslabox\bin\teslabox-linux-arm64`。

## 日常升级

先从 Windows 上传新二进制。上传到 `/tmp` 不会覆盖正在运行的版本：

```powershell
scp .\bin\teslabox-linux-arm64 rock@192.0.2.38:/tmp/teslabox.new
ssh rock@192.0.2.38
```

进入板端后，依次执行：

```sh
sha256sum /tmp/teslabox.new
sudo systemctl stop teslabox.service
sudo cp -a /usr/local/bin/teslabox /usr/local/bin/teslabox.backup
sudo install -o root -g root -m 0755 /tmp/teslabox.new /usr/local/bin/teslabox
sudo nano /etc/teslabox/teslabox.env
```

在配置文件中确认这一行存在：

```ini
TESLABOX_HTTP_ADDR=:80
```

不要用示例配置整体覆盖 `/etc/teslabox/teslabox.env`，否则可能丢失旧版 Web 认证兼容配置和数据盘 UUID。新版认证状态保存在 `/var/lib/teslabox-system/web-auth.json`，升级时同样不得删除。保存后继续：

```sh
sudo systemctl start teslabox.service
sudo systemctl --no-pager --full status teslabox.service
sudo journalctl -u teslabox.service -n 80 --no-pager
curl -I http://127.0.0.1/
sudo ss -ltnp '( sport = :80 )'
```

服务正常时，浏览器可直接打开：

- 局域网：`http://192.0.2.38/`
- mDNS：`http://teslabox.local/`
- Tailscale：`http://100.86.240.43/`，地址以板端 `tailscale ip -4` 的实际输出为准

确认新版本正常后删除临时文件：

```sh
rm -f /tmp/teslabox.new
```

## 首次安装 systemd 服务

首次安装时，再把仓库中的服务和配置示例分别上传到 `/tmp`。安装动作保持分步执行，便于确认每一步对系统的影响：

```powershell
scp .\deploy\systemd\teslabox.service rock@192.0.2.38:/tmp/teslabox.service
scp .\configs\teslabox.env.example rock@192.0.2.38:/tmp/teslabox.env.example
```

板端执行：

```sh
sudo install -d -o root -g root -m 0750 /etc/teslabox
sudo install -o root -g root -m 0640 /tmp/teslabox.env.example /etc/teslabox/teslabox.env
sudo install -o root -g root -m 0644 /tmp/teslabox.service /etc/systemd/system/teslabox.service
sudo nano /etc/teslabox/teslabox.env
sudo systemctl daemon-reload
sudo systemctl enable teslabox.service
sudo systemctl start teslabox.service
```

首次启动前必须按实际设备补齐数据盘身份。Web 认证会自动启用，全新安装初始密码为 `teslabox`，首次登录后必须立即修改。数据盘初始化、TeslaCam 镜像创建、udev 规则和 AP 服务是独立步骤，不能仅凭本节命令盲目初始化或格式化存储设备。

## 回滚

如果新版本无法启动：

```sh
sudo systemctl stop teslabox.service
sudo install -o root -g root -m 0755 /usr/local/bin/teslabox.backup /usr/local/bin/teslabox
sudo systemctl start teslabox.service
sudo systemctl --no-pager --full status teslabox.service
```

如果故障只是端口 `80` 被其他服务占用，可先将 `/etc/teslabox/teslabox.env` 中的监听地址临时恢复为 `:8080`，再重启 TeslaBox；此时访问地址也要重新带上 `:8080`。
