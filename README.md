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
# 为方便，下文用 sshd-server 指代模块内的可执行文件：
SSHD=/data/adb/modules/android-sshd/bin/sshd-server

adb shell su -c "cat /data/adb/android-sshd/password.txt"
# 或用 CLI 查看：
adb shell su -c "$SSHD password"
```

3. 首次用密码登录：

```sh
ssh root@<设备IP> -p 2222
```

4. **登录成功后配置公钥**（即时生效，无需重启）。**推荐用带校验的 `add-key`**，它会先解析公钥格式再写入，避免手抄/粘贴时被截断（`cat >>` 方式会静默接受格式错误的行）：

```sh
# 把 id_ed25519.pub 的【整行】（含 ssh-ed25519 前缀）作为参数传给 add-key：
ssh <用户名>@<设备IP> -p 2222 "/data/adb/modules/android-sshd/bin/sshd-server add-key 'ssh-ed25519 AAAA...<完整base64> 你的注释'"
```

> 常见错误：`authorized_keys` 里只留了半截 base64、或丢了 `ssh-ed25519 ` 前缀 → 服务端解析不了 → 公钥登录报 `认证失败`。用 `add-key` 会当场报错提示格式无效，而非默默写入坏数据。登录后可用 `sshd-server status` 或在设备端 `cat /data/adb/android-sshd/authorized_keys` 核对。

5. 之后可用公钥登录；关闭密码登录：`sshd-server passwd ""`（置空 PASSWORD），无需重启即生效。

## 网段限制（STA / 本机网段）

默认 `ALLOW_NET=192.168.0.0/24`：**仅接受来源 IP 在 192.168.0.* 的连接**（例如连接到随身 WiFi 管理网段的 PC）。  
蜂窝网/其它网段、以及非 `192.168.0.*` 的客户端会被直接断开。

- 修改：`sshd-server allow 192.168.0.0/24,192.168.1.0/24`（或编辑 `sshd.conf` 的 `ALLOW_NET`），然后 `sshd-server restart`
- 特殊需要可设为多段，例如 `ALLOW_NET=192.168.0.0/24,192.168.1.0/24`
- 不建议清空该项后暴露公网

## 管理命令（CLI）

所有管理操作内置于单一二进制，**无需依赖 shell 脚本**。设备上直接执行
`/data/adb/modules/android-sshd/bin/sshd-server <命令>`（下文简称 `sshd-server`）：

```sh
sshd-server start            # 后台拉起守护进程（setsid 脱会话）并确认就绪
sshd-server stop             # 经控制套接字优雅停止（回退到信号）
sshd-server restart          # 停止并重新拉起
sshd-server status           # pid / 监听 / 来源 / 活动连接 / 运行时长
sshd-server init             # 仅初始化配置/密钥/随机密码，不启动
sshd-server passwd [新密码]   # 设置密码；省略则随机生成（即时生效）
sshd-server password         # 显示当前密码
sshd-server user <用户名>     # 改用户名（即时生效）
sshd-server port <端口>       # 改端口（需 restart）
sshd-server allow <网段>      # 改来源网段（需 restart）
sshd-server add-key "ssh-ed25519 AAAA... user@host"  # 追加公钥（即时生效）
```

- `USERNAME`/`PASSWORD`/公钥在**每次登录时热加载**，改后即时生效、无需重启
- 改 `SSH_PORT`/`ALLOW_NET` 需 `sshd-server restart`
- 全局标志 `-home` 可指向非默认运行态目录（默认 `/data/adb/android-sshd`）

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
/data/adb/modules/android-sshd/    模块本体（bin/sshd-server、service.sh 仅作开机触发）
/data/adb/android-sshd/            运行态
  sshd.conf                        配置（0600）
  password.txt                     当前登录密码（0600，adb shell 可查看）
  authorized_keys                  用户自行配置的公钥
  ssh_host_ed25519                 主机密钥（自动生成，0600）
  sshd.log                         日志
  sshd.pid                         守护进程 PID（由服务进程自写，准确）
  sshd.sock                        Unix 域控制套接字（0600，status/stop 走此通道）
```

升级模块不会覆盖运行态配置与密钥。管理无需 `restart.sh`，全部由 `sshd-server` CLI 完成。

## 安全须知

- SSH 以 **root** 运行；密码为安装随机生成，仍须防止 password.txt 被无关人员读取
- 建议首次登录后尽快配置公钥；不需要密码时将 `PASSWORD=` 置空
- 默认仅允许 `192.168.0.*` 来源，请勿把 `ALLOW_NET` 清空后暴露公网
- 不要在仓库或共享目录保存设备私钥

## 源码与构建

- 源码 `android-sshd/*.go`（单二进制多子命令：`main`/`config`/`keys`/`server`/`control`/`daemon`；依赖 x/crypto/ssh + creack/pty + pkg/sftp）
- 本地交叉编译（Windows → aarch64）并打包：
  ```powershell
  cd android-sshd
  $env:GOOS="linux"; $env:GOARCH="arm64"; $env:CGO_ENABLED="0"
  go build -trimpath -ldflags "-s -w" -o ../module/android-sshd/bin/sshd-server .
  cd ..
  python make_zip.py   # 读 module.prop 的 version，输出 dist/android-sshd-<version>.zip
  ```
- 配置项均可用命令行参数覆盖：
  `-home` `-listen` `-allow` `-authorized-keys` `-host-key` `-config`

## 发布流水线（GitHub Actions）

`.github/workflows/release.yml`：**推送 `v*` 标签**时自动交叉编译 linux/arm64 → 用 `make_zip.py` 打包 Magisk zip → 以该标签**创建 GitHub Release** 并附上 zip。也可在 Actions 页手动触发（`workflow_dispatch`）下载构建工件。

发布步骤：
1. 修改代码后，更新 `module/android-sshd/module.prop` 的 `version`/`versionCode`，并同步 `main.go` 的 `versionString`；
2. 提交并推送到默认分支，再打 tag 并推送：`git tag v1.1.0 && git push origin v1.1.0`；
3. 流水线自动创建 Release 并上传 `android-sshd-<version>.zip`。
