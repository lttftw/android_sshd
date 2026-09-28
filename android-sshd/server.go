package main

import (
	"crypto/subtle"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

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

// handshakeTimeout 是完成 SSH 传输层握手允许的较长时间，防止慢连接占用资源。
const handshakeTimeout = 20 * time.Second

// server 承载一个 SSH 服务实例的运行态：监听、活动连接登记、控制套接字与优雅退出。
type server struct {
	paths     Paths
	cfg       Config
	allowNets []*net.IPNet
	listen    string

	sshCfg *ssh.ServerConfig

	ln  net.Listener
	ctl net.Listener

	mu        sync.Mutex
	conns     map[net.Conn]struct{}
	connCount int64

	started  time.Time
	stopOnce sync.Once
	stopCh   chan struct{}
}

func newServer(paths Paths, cfg Config, listen, allow string) (*server, error) {
	nets, err := parseAllowNets(allow)
	if err != nil {
		return nil, fmt.Errorf("来源网段: %w", err)
	}

	signer, err := loadOrCreateHostKey(paths.HostKey)
	if err != nil {
		return nil, fmt.Errorf("主机密钥: %w", err)
	}

	confPath := paths.Conf
	authPath := paths.AuthKeys
	if len(loadAuthorizedKeys(authPath)) == 0 {
		log.Printf("提示: %s 为空，仅密码可登录（首次密码登录后请自行添加公钥；添加后即时生效，无需重启）", authPath)
	}

	s := &server{
		paths:     paths,
		cfg:       cfg,
		allowNets: nets,
		listen:    listen,
		conns:     map[net.Conn]struct{}{},
		started:   time.Now(),
		stopCh:    make(chan struct{}),
	}

	sshCfg := &ssh.ServerConfig{
		MaxAuthTries: 6,
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			// 每次鉴权实时读配置与公钥：改用户名/公钥无需重启。
			c := loadConfig(confPath)
			if conn.User() != c.Username {
				return nil, fmt.Errorf("认证失败")
			}
			raw := string(key.Marshal())
			for _, ak := range loadAuthorizedKeys(authPath) {
				if string(ak.Marshal()) == raw {
					return &ssh.Permissions{}, nil
				}
			}
			return nil, fmt.Errorf("认证失败")
		},
		PasswordCallback: func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			c := loadConfig(confPath)
			if c.Password != "" && conn.User() == c.Username &&
				subtle.ConstantTimeCompare([]byte(c.Password), password) == 1 {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("认证失败")
		},
	}
	sshCfg.AddHostKey(signer)
	s.sshCfg = sshCfg
	return s, nil
}

// serve 启动 TCP 监听与控制套接字，登记 PID，进入接受循环，直到收到停止信号。
func (s *server) serve() error {
	ln, err := net.Listen("tcp", s.listen)
	if err != nil {
		return fmt.Errorf("监听失败: %w", err)
	}
	s.ln = ln

	_ = os.Remove(s.paths.Sock)
	ctl, err := net.Listen("unix", s.paths.Sock)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("控制套接字: %w", err)
	}
	s.ctl = ctl

	if err := os.WriteFile(s.paths.PID, []byte(fmt.Sprintf("%d", os.Getpid())), 0o600); err != nil {
		log.Printf("写 PID 文件失败: %v", err)
	}

	go s.acceptLoop()
	go s.serveControl()

	log.Printf("Android SSH 服务器已启动，监听 %s（pid=%d，公钥+密码登录，用户名 %s）", s.listen, os.Getpid(), s.cfg.Username)
	if len(s.allowNets) == 0 {
		log.Printf("来源限制: 不限制（请勿暴露公网）")
	} else {
		log.Printf("来源限制: 仅允许 %s", s.cfg.AllowNet)
	}
	log.Printf("连接示例: ssh %s@<设备IP> -p %s", s.cfg.Username, portOf(s.listen))

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-s.stopCh:
		log.Printf("收到停止请求（控制命令）")
	case sig := <-sigCh:
		log.Printf("收到信号 %v，正在退出", sig)
	}
	s.shutdown()
	return nil
}

func (s *server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return
			default:
				log.Printf("接受连接失败: %v", err)
				continue
			}
		}
		if !remoteAllowed(conn.RemoteAddr(), s.allowNets) {
			log.Printf("已拒绝非允许网段连接: %s", conn.RemoteAddr())
			_ = conn.Close()
			continue
		}
		go s.handleConn(conn)
	}
}

func (s *server) track(c net.Conn) {
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.mu.Unlock()
	atomic.AddInt64(&s.connCount, 1)
}

func (s *server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	atomic.AddInt64(&s.connCount, -1)
}

// shutdown 幂等地关闭监听、控制套接字与全部活动连接，并清理 PID/套接字文件。
func (s *server) shutdown() {
	s.stopOnce.Do(func() { close(s.stopCh) })
	if s.ln != nil {
		_ = s.ln.Close()
	}
	if s.ctl != nil {
		_ = s.ctl.Close()
	}
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	_ = os.Remove(s.paths.PID)
	_ = os.Remove(s.paths.Sock)
	log.Printf("已停止（goodbye）")
}

func (s *server) handleConn(conn net.Conn) {
	defer func() {
		s.untrack(conn)
		_ = conn.Close()
	}()
	s.track(conn)

	// 握手阶段设超时，防慢连接；成功后清除。
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetDeadline(time.Now().Add(handshakeTimeout))
	}
	sconn, chans, reqs, err := ssh.NewServerConn(conn, s.sshCfg)
	if err != nil {
		log.Printf("SSH 握手失败 %s: %v", conn.RemoteAddr(), err)
		return
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetDeadline(time.Time{})
	}
	log.Printf("用户 %s 登录自 %s", sconn.User(), conn.RemoteAddr())

	go ssh.DiscardRequests(reqs)
	for newChan := range chans {
		switch newChan.ChannelType() {
		case "session":
			go s.handleSession(newChan)
		case "direct-tcpip":
			go s.handleDirectTCPIP(newChan)
		default:
			newChan.Reject(ssh.UnknownChannelType, "不支持的通道类型")
		}
	}
}

// 本地端口转发 (-L)：连接目标并双向转发字节。
func (s *server) handleDirectTCPIP(newChan ssh.NewChannel) {
	var req struct {
		DestAddr string
		DestPort uint32
	}
	if err := ssh.Unmarshal(newChan.ExtraData(), &req); err != nil {
		newChan.Reject(ssh.ConnectionFailed, "bad forward request")
		return
	}
	target := net.JoinHostPort(req.DestAddr, fmt.Sprintf("%d", req.DestPort))
	upstream, err := net.DialTimeout("tcp", target, 10*time.Second)
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

func (s *server) handleSession(newChan ssh.NewChannel) {
	ch, reqs, err := newChan.Accept()
	if err != nil {
		return
	}
	defer ch.Close()

	var (
		term                     string
		pendingCols, pendingRows uint32
		cmdEnv                   []string
		once                     sync.Once
		ptmx                     *os.File
		mu                       sync.Mutex
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
				Term    string
				Columns uint32
				Rows    uint32
				Width   uint32
				Height  uint32
				Modes   string
			}
			// 即使解析出错也回复成功，避免客户端等待 pty-req 确认而挂起。
			if err := ssh.Unmarshal(req.Payload, &p); err == nil {
				term = p.Term
				pendingCols, pendingRows = p.Columns, p.Rows
				if pendingCols == 0 {
					pendingCols = p.Width / 8
				}
				if pendingRows == 0 {
					pendingRows = p.Height / 16
				}
				applySize()
			}
			_ = req.Reply(true, nil)
		case "window-change":
			var w struct {
				Columns uint32
				Rows    uint32
				Width   uint32
				Height  uint32
			}
			if err := ssh.Unmarshal(req.Payload, &w); err == nil {
				pendingCols, pendingRows = w.Columns, w.Rows
				if pendingCols == 0 {
					pendingCols = w.Width / 8
				}
				if pendingRows == 0 {
					pendingRows = w.Height / 16
				}
				applySize()
			}
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
			var sub struct{ Name string }
			if err := ssh.Unmarshal(req.Payload, &sub); err != nil || sub.Name != "sftp" {
				_ = req.Reply(false, nil)
				continue
			}
			// 提供 SFTP 文件传输（sftp 客户端），以 root 访问整个文件系统。
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
