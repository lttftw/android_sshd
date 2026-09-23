// Android 无屏 SSH 服务器（通用 Magisk 模块，独立于任何代理模块）
// 提供以 root 登录的真实终端，支持公钥与密码双鉴权、SFTP 文件传输与本地端口转发。
// 用法: sshd-server -listen 0.0.0.0:2222 [-allow 192.168.0.0/24] [-config 文件] [-authorized-keys 文件] [-host-key 文件]
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/creack/pty"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// 默认 PATH：常见系统目录（通用，不依赖任何特定模块）。
const defaultPath = "/system/bin:/vendor/bin:/sbin:/system/sbin:/system/xbin"

func shellEnv(extra []string) []string {
	env := []string{"PATH=" + defaultPath}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "PATH=") {
			env = append(env, e)
		}
	}
	return append(env, extra...)
}

func main() {
	listen := flag.String("listen", "0.0.0.0:2222", "监听地址")
	allowNet := flag.String("allow", "192.168.0.0/24", "允许连接的来源网段，逗号分隔；空字符串表示不限制（默认仅 STA 管理网段 192.168.0.0/24）")
	authFile := flag.String("authorized-keys", "/data/adb/android-sshd/authorized_keys", "公钥文件（authorized_keys 格式）")
	hostKeyFile := flag.String("host-key", "/data/adb/android-sshd/ssh_host_ed25519", "主机密钥文件（不存在则自动生成）")
	confFile := flag.String("config", "/data/adb/android-sshd/sshd.conf", "配置文件（USERNAME/PASSWORD）")
	flag.Parse()

	allowNets, err := parseAllowNets(*allowNet)
	if err != nil {
		log.Fatal("来源网段: ", err)
	}

	cfg := loadConfig(*confFile)

	signer, err := loadOrCreateHostKey(*hostKeyFile)
	if err != nil {
		log.Fatal("主机密钥: ", err)
	}

	authorized := loadAuthorizedKeys(*authFile)
	if len(authorized) == 0 {
		log.Printf("提示: %s 为空，仅密码可登录（首次密码登录后请自行添加公钥；添加后即时生效，无需重启）", *authFile)
	}

	config := &ssh.ServerConfig{
		MaxAuthTries: 6,
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			// 每次鉴权实时读配置与公钥：改用户名/公钥无需重启。
			cfg := loadConfig(*confFile)
			if conn.User() != cfg.username {
				return nil, fmt.Errorf("认证失败")
			}
			raw := string(key.Marshal())
			for _, ak := range loadAuthorizedKeys(*authFile) {
				if string(ak.Marshal()) == raw {
					return &ssh.Permissions{}, nil
				}
			}
			return nil, fmt.Errorf("认证失败")
		},
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			cfg := loadConfig(*confFile)
			if cfg.password != "" && conn.User() == cfg.username &&
				subtle.ConstantTimeCompare([]byte(cfg.password), password) == 1 {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("认证失败")
		},
	}
	config.AddHostKey(signer)

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal("监听失败: ", err)
	}
	log.Printf("Android SSH 服务器已启动，监听 %s（公钥+密码登录，用户名 %s）", *listen, cfg.username)
	if len(allowNets) == 0 {
		log.Printf("来源限制: 不限制（请勿暴露公网）")
	} else {
		log.Printf("来源限制: 仅允许 %s（STA/本机管理网段）", *allowNet)
	}
	log.Printf("连接示例: ssh %s@<设备IP> -p %s", cfg.username, portOf(*listen))

	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("接受连接失败: %v", err)
			continue
		}
		if !remoteAllowed(conn.RemoteAddr(), allowNets) {
			log.Printf("已拒绝非允许网段连接: %s", conn.RemoteAddr())
			_ = conn.Close()
			continue
		}
		go handleConn(conn, config)
	}
}

// parseAllowNets 解析逗号分隔的 CIDR 列表；空白/空串表示不限制。
func parseAllowNets(s string) ([]*net.IPNet, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var nets []*net.IPNet
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// 允许写单个 IP，按 /32 或 /128 处理
		if ip := net.ParseIP(part); ip != nil {
			if ip.To4() != nil {
				part += "/32"
			} else {
				part += "/128"
			}
		}
		_, n, err := net.ParseCIDR(part)
		if err != nil {
			return nil, fmt.Errorf("无效网段 %q: %v", part, err)
		}
		nets = append(nets, n)
	}
	return nets, nil
}

// remoteAllowed 判断远端地址是否落在允许网段内。
func remoteAllowed(addr net.Addr, allowNets []*net.IPNet) bool {
	if len(allowNets) == 0 {
		return true
	}
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return false
	}
	ip := tcp.IP
	if ip == nil {
		return false
	}
	for _, n := range allowNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func handleConn(conn net.Conn, config *ssh.ServerConfig) {
	sconn, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		log.Printf("SSH 握手失败 %s: %v", conn.RemoteAddr(), err)
		return
	}
	log.Printf("用户 %s 登录自 %s", sconn.User(), conn.RemoteAddr())

	go ssh.DiscardRequests(reqs)
	for newChan := range chans {
		switch newChan.ChannelType() {
		case "session":
			go handleSession(newChan)
		case "direct-tcpip":
			go handleDirectTCPIP(newChan)
		default:
			newChan.Reject(ssh.UnknownChannelType, "不支持的通道类型")
		}
	}
}

// 本地端口转发 (-L)：连接目标并双向转发字节。
func handleDirectTCPIP(newChan ssh.NewChannel) {
	var req struct {
		DestAddr string
		DestPort uint32
	}
	if err := ssh.Unmarshal(newChan.ExtraData(), &req); err != nil {
		newChan.Reject(ssh.ConnectionFailed, "bad forward request")
		return
	}
	target := fmt.Sprintf("%s:%d", req.DestAddr, req.DestPort)
	upstream, err := net.Dial("tcp", target)
	if err != nil {
		newChan.Reject(ssh.ConnectionFailed, "无法连接目标: "+target)
		return
	}
	defer upstream.Close()

	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(ch, upstream); _ = ch.CloseWrite() }()
	go func() { defer wg.Done(); _, _ = io.Copy(upstream, ch) }()
	wg.Wait()
}

func handleSession(newChan ssh.NewChannel) {
	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}
	defer ch.Close()

	var (
		term                      string
		pendingCols, pendingRows  uint32
		cmdEnv                    []string
		once                      sync.Once
		ptmx                      *os.File
		mu                        sync.Mutex
	)

	applySize := func() {
		mu.Lock()
		defer mu.Unlock()
		if ptmx != nil && pendingCols > 0 && pendingRows > 0 {
			_ = pty.Setsize(ptmx, &pty.Winsize{Rows: uint16(pendingRows), Cols: uint16(pendingCols)})
		}
	}

	startShell := func() error {
		var runErr error
		once.Do(func() {
			c := exec.Command("/system/bin/sh")
			c.Env = shellEnv(cmdEnv)
			if term == "" {
				term = "xterm"
			}
			c.Env = append(c.Env, "TERM="+term)
			pt, err := pty.Start(c)
			if err != nil {
				runErr = err
				return
			}
			ptmx = pt
			applySize()
			go func() { _, _ = io.Copy(ch, pt); _ = ch.CloseWrite() }()
			go func() { _, _ = io.Copy(pt, ch); _ = pt.Close() }()
		})
		return runErr
	}

	for req := range reqs {
		switch req.Type {
		case "pty-req":
			var p struct {
				Term     string
				Columns  uint32
				Rows     uint32
				Width    uint32
				Height   uint32
				Modes    string
			}
			if err := ssh.Unmarshal(req.Payload, &p); err != nil {
				continue
			}
			term = p.Term
			pendingCols, pendingRows = p.Columns, p.Rows
			if pendingCols == 0 {
				pendingCols = p.Width / 8
			}
			if pendingRows == 0 {
				pendingRows = p.Height / 16
			}
			applySize()
			_ = req.Reply(true, nil)
		case "window-change":
			var w struct {
				Columns uint32
				Rows    uint32
				Width   uint32
				Height  uint32
			}
			if err := ssh.Unmarshal(req.Payload, &w); err != nil {
				continue
			}
			pendingCols, pendingRows = w.Columns, w.Rows
			if pendingCols == 0 {
				pendingCols = w.Width / 8
			}
			if pendingRows == 0 {
				pendingRows = w.Height / 16
			}
			applySize()
		case "env":
			var kv struct{ Name, Value string }
			if err := ssh.Unmarshal(req.Payload, &kv); err == nil {
				cmdEnv = append(cmdEnv, kv.Name+"="+kv.Value)
			}
			_ = req.Reply(true, nil)
		case "shell":
			if err := startShell(); err != nil {
				_ = req.Reply(false, nil)
				_, _ = ch.Write([]byte("无法启动 Shell: " + err.Error() + "\r\n"))
				return
			}
			_ = req.Reply(true, nil)
		case "exec":
			var e struct{ Command string }
			if err := ssh.Unmarshal(req.Payload, &e); err != nil {
				continue
			}
			c := exec.Command("/system/bin/sh", "-c", e.Command)
			c.Env = shellEnv(cmdEnv)
			out, err := c.CombinedOutput()
			exitStatus := 0
			if err != nil {
				if ee, ok := err.(*exec.ExitError); ok {
					exitStatus = ee.ExitCode()
				} else {
					exitStatus = 1
				}
			}
			_, _ = ch.Write(out)
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(exitStatus)}))
			_ = req.Reply(true, nil)
			return
		case "subsystem":
			var s struct{ Name string }
			if err := ssh.Unmarshal(req.Payload, &s); err != nil || s.Name != "sftp" {
				_ = req.Reply(false, nil)
				continue
			}
			// 提供 SFTP 文件传输（scp/sftp 客户端），以 root 访问整个文件系统。
			_ = req.Reply(true, nil)
			srv, err := sftp.NewServer(ch)
			if err != nil {
				return
			}
			_ = srv.Serve()
			return
		}
	}
}

// 加载主机密钥；不存在则生成 ed25519 密钥并写入（0600）。
// 注意: 生成的是 PKCS8；x/crypto 的 ParseRawPrivateKey 对部分环境不可靠，
// 必须先走 x509.ParsePKCS8PrivateKey，否则每次重启都会误判“损坏”而换钥
// （导致客户端 Host key verification failed，表现为连不上）。
func loadOrCreateHostKey(path string) (ssh.Signer, error) {
	if b, err := os.ReadFile(path); err == nil {
		if block, _ := pem.Decode(b); block != nil {
			if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
				return ssh.NewSignerFromKey(key)
			}
			if key, err := ssh.ParseRawPrivateKey(block.Bytes); err == nil {
				return ssh.NewSignerFromKey(key)
			}
			log.Printf("主机密钥文件无法解析，将重新生成: %s", path)
		}
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	log.Printf("已生成新的主机密钥: %s", path)
	return ssh.NewSignerFromKey(priv)
}

func loadAuthorizedKeys(path string) []ssh.PublicKey {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var keys []ssh.PublicKey
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line)); err == nil {
			keys = append(keys, pk)
		}
	}
	return keys
}

type fileConfig struct {
	username string
	password string
}

// 读取 key=value 形式的配置文件（# 开头为注释）。缺失时返回默认值。
// 每行去掉 \r，兼容 Windows 编辑；PASSWORD 不做额外语义处理（仅 TrimSpace 两端）。
func loadConfig(path string) fileConfig {
	cfg := fileConfig{username: "root", password: ""}
	if path == "" {
		return cfg
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.ReplaceAll(line, "\r", "")
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "USERNAME":
			cfg.username = strings.TrimSpace(v)
		case "PASSWORD":
			cfg.password = strings.TrimSpace(v)
		}
	}
	return cfg
}

func portOf(listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		return listen[i+1:]
	}
	return listen
}
