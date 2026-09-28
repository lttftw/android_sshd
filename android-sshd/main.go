// Android 无屏 SSH 服务器（通用 Magisk 模块，独立于任何代理模块）
//
// 单一二进制内区分两类进程：
//   - 服务进程：`sshd-server run`（前台）/ `sshd-server start`（setsid 后台守护）
//   - 管理进程：`sshd-server stop|restart|status|passwd|user|port|allow|add-key|...`
//
// 管理通过 PID 文件 + Unix 域控制套接字完成，无需依赖 shell 脚本逻辑；
// Magisk 开机脚本只需一行 `sshd-server start`。
//
// 提供以 root 登录的真实终端，支持公钥与密码双鉴权、SFTP 文件传输与本地端口转发。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

const versionString = "v1.1.0"

func usage() {
	fmt.Fprint(os.Stderr, `Android SSH Server `+versionString+`

用法: sshd-server [全局标志] <命令> [命令参数]

全局标志:
  -home 路径            运行态目录 (默认 `+DefaultHome+`)
  -listen 地址          监听地址 (默认由配置 SSH_PORT 推导)
  -allow 网段           允许来源 CIDR，逗号分隔 (默认由配置 ALLOW_NET 推导)
  -config 文件          配置文件 (默认 home/sshd.conf)
  -authorized-keys 文件 公钥文件 (默认 home/authorized_keys)
  -host-key 文件        主机密钥 (默认 home/ssh_host_ed25519)

命令:
  run            前台运行服务 (无命令时的默认行为)
  start          后台拉起守护进程并确认就绪
  stop           优雅停止服务
  restart        停止并重新拉起
  status         查看运行状态 (pid/监听/来源/活动连接/运行时长)
  init           仅初始化配置/密钥/随机密码，不启动
  passwd [新密码] 设置登录密码；省略则生成随机密码 (即时生效)
  password       显示当前登录密码
  user <用户名>   设置登录用户名 (即时生效)
  port <端口>     设置监听端口 (需 restart)
  allow <网段>    设置允许来源网段 (需 restart)
  add-key <公钥>  追加一条公钥到 authorized_keys (即时生效)
  version        显示版本
  help           显示本帮助

示例:
  sshd-server start
  sshd-server status
  sshd-server passwd
  sshd-server add-key "ssh-ed25519 AAAA... user@host"
  sshd-server restart
`)
}

func firstArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func main() {
	log.SetFlags(log.LstdFlags)

	args := os.Args[1:]
	sub := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub = args[0]
		args = args[1:]
	}

	fs := flag.NewFlagSet(sub, flag.ExitOnError)
	var o options
	fs.StringVar(&o.home, "home", DefaultHome, "运行态目录")
	fs.StringVar(&o.listen, "listen", "", "监听地址")
	fs.StringVar(&o.allow, "allow", "", "允许来源网段")
	fs.StringVar(&o.conf, "config", "", "配置文件")
	fs.StringVar(&o.authKeys, "authorized-keys", "", "公钥文件")
	fs.StringVar(&o.hostKey, "host-key", "", "主机密钥文件")
	_ = fs.Parse(args)
	rest := fs.Args()

	provided := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { provided[f.Name] = true })
	o.listenSet = provided["listen"]
	o.allowSet = provided["allow"]

	var err error
	switch sub {
	case "run":
		err = cmdRun(o)
	case "start":
		err = cmdStart(o)
	case "stop":
		err = cmdStop(o)
	case "restart":
		err = cmdRestart(o)
	case "_restart_worker": // 内部：setsid 脱离会话后台执行真正的 stop+start
		err = doRestart(o)
	case "status":
		err = cmdStatus(o)
	case "init":
		err = cmdInit(o)
	case "passwd":
		err = cmdPasswd(o, firstArg(rest))
	case "password":
		err = cmdShowPassword(o)
	case "user":
		err = cmdSetUser(o, firstArg(rest))
	case "port":
		err = cmdSetPort(o, firstArg(rest))
	case "allow":
		err = cmdSetAllow(o, firstArg(rest))
	case "add-key":
		err = cmdAddKey(o, strings.Join(rest, " "))
	case "version":
		fmt.Println(versionString)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", sub)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}
