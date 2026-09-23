# Android SSH Server（android-sshd）Magisk 模块

通用 SSH 服务器，适配任意已 Root 的 Android 设备（无屏随身 WiFi / 软路由盒子 / 开发板）。
提供 **root 交互终端 + SFTP 文件传输 + 本地端口转发**，开机自启，与任何代理/其他模块无关。

## 功能

| 能力 | 说明 |
|---|---|
| SSH 终端 | root 交互 shell，密码 + 公钥双鉴权 |
| SFTP 文件传输 | `sftp` 客户端双向传文件 |
| 本地端口转发 | `ssh -L` 把设备上任意端口映射到本机 |
| 非交互命令 | `ssh host "命令"` 直接执行 |
| 开机自启 | Magisk `service.sh` 自动拉起 |
| 来源限制 | 默认仅允许 `192.168.0.0/24`（STA/本机管理网段） |
| 随机密码 | 安装/首启生成，无默认口令；公钥由用户自行配置 |

## 安装

```sh
adb push android-sshd.zip /data/local/tmp/
adb shell su -c "magisk --install-module /data/local/tmp/android_sshd.zip"
# 重启设备生效；或立即启动：
adb shell su -c "sh /data/adb/modules/android-sshd/service.sh"
```

## 登录方式（首次密码 → 之后可改公钥）

模块**不内置任何密钥对**，也**没有固定默认密码**。

1. 安装或首次启动时生成随机密码，写入运行态与日志。
2. 在设备上查看密码（随身 WiFi 的 adb shell）：

```sh
adb shell su -c "cat /data/adb/android-sshd/password.txt"
# 或
adb shell su -c "sh /data/adb/android-sshd/restart.sh password"
# 日志中也有一条 [sshd-init]/[install] 提示：
adb shell su -c "grep -n password /data/adb/android-sshd/sshd.log"
```

3. 首次用密码登录：

```sh
ssh root@<设备IP> -p 2222
```

4. **登录成功后自行配置公钥**（每行一个，即时生效，无需重启）：

```sh
# 在 PC 上把公钥追加到设备
type %USERPROFILE%\.ssh\id_ed25519.pub | ssh root@<设备IP> -p 2222 "cat >> /data/adb/android-sshd/authorized_keys && chmod 600 /data/adb/android-sshd/authorized_keys"
```

5. 之后可用公钥登录；可将 `sshd.conf` 中 `PASSWORD=` 置空以关闭密码登录，然后 `restart.sh restart`。

## 网段限制（STA / 本机网段）

默认 `ALLOW_NET=192.168.0.0/24`：**仅接受来源 IP 在 192.168.0.* 的连接**（例如连接到随身 WiFi 管理网段的 PC）。  
蜂窝网/其它网段、以及非 `192.168.0.*` 的客户端会被直接断开。

- 修改：编辑 `/data/adb/android-sshd/sshd.conf` 的 `ALLOW_NET`（逗号可写多个 CIDR），然后重启服务
- 特殊需要可设为多段，例如 `ALLOW_NET=192.168.0.0/24,192.168.1.0/24`
- 不建议清空该项后暴露公网

## 其它配置

```
USERNAME=root          # 密码/公钥登录均强制校验的用户名
PASSWORD=<随机>        # 置空则关闭密码登录
SSH_PORT=2222
ALLOW_NET=192.168.0.0/24
```

- 配置文件兼容 Windows 编辑产生的 CRLF 行尾
- 支持密钥类型：ed25519、RSA、ECDSA 等 OpenSSH 标准公钥

## 使用

```sh
# 终端
ssh root@<设备IP> -p 2222

# 公钥登录
ssh root@<设备IP> -p 2222 -i <私钥>

# 传文件
sftp -P 2222 root@<设备IP>

# 端口转发
ssh -L 9090:127.0.0.1:9090 root@<设备IP> -p 2222

# 非交互执行
ssh root@<设备IP> -p 2222 "id; ls /data/adb"
```

## 目录结构

```
/data/adb/modules/android-sshd/    模块本体（bin/sshd-server、service.sh）
/data/adb/android-sshd/            运行态
  sshd.conf                        配置（0600）
  password.txt                     当前登录密码（0600，adb shell 可查看）
  authorized_keys                  用户自行配置的公钥
  ssh_host_ed25519                 主机密钥（自动生成，0600）
  sshd.log                         日志
  sshd.pid                         进程 PID
  restart.sh                       {restart|stop|status|password}
```

升级模块不会覆盖运行态配置与密钥。

## 安全须知

- SSH 以 **root** 运行；密码为安装随机生成，仍须防止 password.txt 被无关人员读取
- 建议首次登录后尽快配置公钥；不需要密码时将 `PASSWORD=` 置空
- 默认仅允许 `192.168.0.*` 来源，请勿把 `ALLOW_NET` 清空后暴露公网
- 不要在仓库或共享目录保存设备私钥

## 源码与重编译

- `android-sshd/main.go`（Go：x/crypto/ssh + creack/pty + pkg/sftp）
- 交叉编译（Windows → aarch64）：
  ```powershell
  $env:GOOS="linux"; $env:GOARCH="arm64"; $env:CGO_ENABLED="0"
  go build -trimpath -ldflags "-s -w" -o ../module/android-sshd/bin/sshd-server .
  ```
- 配置项均可用命令行参数覆盖：
  `-listen` `-allow` `-authorized-keys` `-host-key` `-config`
