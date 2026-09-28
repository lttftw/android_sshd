package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// ensureInit 保证运行态目录、配置、密码、主机密钥与 authorized_keys 就位：
// 缺失则创建（首次生成随机密码），已存在则原样保留。幂等，可被 start/run 反复调用。
func ensureInit(p Paths) error {
	if err := os.MkdirAll(p.Home, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(p.Conf); os.IsNotExist(err) {
		pw := genRandomPassword()
		lines := []string{
			"# Android SSH Server 配置",
			"# 改 USERNAME/PASSWORD 后无需重启（登录时热加载）；改 SSH_PORT/ALLOW_NET 需 restart",
			"USERNAME=root",
			"PASSWORD=" + pw,
			"SSH_PORT=2222",
			"ALLOW_NET=192.168.0.0/24",
		}
		if err := os.WriteFile(p.Conf, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			return err
		}
		if pw != "" {
			_ = os.WriteFile(p.Password, []byte(pw), 0o600)
		}
		fmt.Printf("[init] 已生成随机登录密码，查看: cat %s\n", p.Password)
		fmt.Printf("[init] 首次密码登录后请写入公钥: %s\n", p.AuthKeys)
	}
	// 主机密钥预生成，避免首次连接竞态
	if _, err := loadOrCreateHostKey(p.HostKey); err != nil {
		return fmt.Errorf("主机密钥: %w", err)
	}
	if _, err := os.Stat(p.AuthKeys); os.IsNotExist(err) {
		_ = os.WriteFile(p.AuthKeys, nil, 0o600)
	} else {
		_ = os.Chmod(p.AuthKeys, 0o600)
	}
	_ = os.Chmod(p.Conf, 0o600)
	return nil
}

// resolveListen 依据覆盖标志与配置计算监听地址与允许网段。
func resolveListen(o options, cfg Config) (listen, allow string) {
	listen = "0.0.0.0:" + cfg.Port
	if o.listenSet {
		listen = o.listen
	}
	allow = cfg.AllowNet
	if o.allowSet {
		allow = o.allow
	}
	return
}

// cmdRun 前台运行服务（也是 start 拉起的守护进程主体）。
func cmdRun(o options) error {
	p := o.runtimePaths()
	if err := ensureInit(p); err != nil {
		return err
	}
	cfg := loadConfig(p.Conf)
	listen, allow := resolveListen(o, cfg)
	srv, err := newServer(p, cfg, listen, allow)
	if err != nil {
		return err
	}
	return srv.serve()
}

// cmdStart 以独立会话（setsid）后台拉起 run 子进程，并通过控制套接字确认就绪。
func cmdStart(o options) error {
	p := o.runtimePaths()
	if err := ensureInit(p); err != nil {
		return err
	}
	if pingSock(p.Sock) {
		st, _ := queryStatus(p.Sock)
		fmt.Printf("已在运行 (pid=%d, listen=%s)\n", st.PID, st.Listen)
		return nil
	}

	self, err := os.Executable()
	if err != nil || self == "" {
		self = os.Args[0]
	}

	cfg := loadConfig(p.Conf)
	listen, allow := resolveListen(o, cfg)
	args := []string{"run", "-home", p.Home, "-listen", listen, "-allow", allow}
	if o.conf != "" {
		args = append(args, "-config", o.conf)
	}
	if o.authKeys != "" {
		args = append(args, "-authorized-keys", o.authKeys)
	}
	if o.hostKey != "" {
		args = append(args, "-host-key", o.hostKey)
	}

	logf, err := os.OpenFile(p.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()

	cmd := exec.Command(self, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // 脱离当前会话，随父进程退出后存活
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动失败: %w", err)
	}
	_ = cmd.Process.Release()

	// 轮询控制套接字确认就绪（PID 由子进程自写，绝对准确）
	for i := 0; i < 30; i++ {
		if pingSock(p.Sock) {
			st, _ := queryStatus(p.Sock)
			fmt.Printf("已启动 (pid=%d, 用户=%s, 端口=%s, 来源=%s)\n",
				st.PID, st.Username, portOf(st.Listen), orNone(st.Allow))
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("启动后未在 3 秒内就绪，请查看日志: %s", p.Log)
}

// cmdStop 优先经控制套接字优雅停止；回退到 PID 文件信号。
func cmdStop(o options) error {
	p := o.runtimePaths()
	pid := readPID(p.PID)

	if pingSock(p.Sock) {
		_, _ = controlRequest(p.Sock, "STOP")
	} else if pid > 0 && processAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	} else {
		fmt.Println("未运行")
		_ = os.Remove(p.Sock)
		_ = os.Remove(p.PID)
		return nil
	}

	for i := 0; i < 40; i++ {
		if !processAlive(pid) && !pingSock(p.Sock) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	_ = os.Remove(p.Sock)
	_ = os.Remove(p.PID)
	fmt.Println("已停止")
	return nil
}

// cmdRestart 重启服务。
//
// 关键：从 SSH 会话内执行 restart 时，进程挂在 sshd-server 守护的会话下；
// 若前台直接 stop，守护 shutdown 会关闭所有连接（含当前终端），pty 主端关闭
// 触发 SIGHUP 杀掉本前台进程，随后的 start 便跑不到，导致“停了却起不来”。
// 因此当服务在运行时，派生一个 setsid 脱离会话的后台 worker 去做真正的 stop+start。
func cmdRestart(o options) error {
	p := o.runtimePaths()
	if !pingSock(p.Sock) && !processAlive(readPID(p.PID)) {
		return cmdStart(o) // 本就未运行：直接启动，无需脱离会话
	}
	fmt.Println("已提交后台重启（约 1~2 秒生效）。若经 SSH 连接，当前会话会断开，属正常，请稍候重连。")
	return spawnRestartWorker(o)
}

// doRestart 是 worker 真正执行的逻辑：先停（等待端口释放）后起。
func doRestart(o options) error {
	p := o.runtimePaths()
	if pingSock(p.Sock) || processAlive(readPID(p.PID)) {
		if err := cmdStop(o); err != nil {
			return err
		}
	}
	time.Sleep(200 * time.Millisecond) // 兜底：确保监听端口彻底释放
	return cmdStart(o)
}

// spawnRestartWorker 以 setsid 启动一个脱离当前会话的自身子进程执行 _restart_worker，
// 其标准输出重定向到日志，随即使守护停服销毁终端也不会波及该 worker。
func spawnRestartWorker(o options) error {
	p := o.runtimePaths()
	self, err := os.Executable()
	if err != nil || self == "" {
		self = os.Args[0]
	}
	args := []string{"_restart_worker", "-home", p.Home}
	if o.conf != "" {
		args = append(args, "-config", o.conf)
	}
	if o.authKeys != "" {
		args = append(args, "-authorized-keys", o.authKeys)
	}
	if o.hostKey != "" {
		args = append(args, "-host-key", o.hostKey)
	}
	if o.listenSet {
		args = append(args, "-listen", o.listen)
	}
	if o.allowSet {
		args = append(args, "-allow", o.allow)
	}

	logf, err := os.OpenFile(p.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()

	cmd := exec.Command(self, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("派生重启任务失败: %w", err)
	}
	_ = cmd.Process.Release()
	return nil
}

func cmdStatus(o options) error {
	p := o.runtimePaths()
	if st, ok := queryStatus(p.Sock); ok {
		fmt.Printf("状态: 运行中\n")
		fmt.Printf("  PID:    %d\n", st.PID)
		fmt.Printf("  监听:   %s\n", st.Listen)
		fmt.Printf("  用户:   %s\n", st.Username)
		fmt.Printf("  来源:   %s\n", orNone(st.Allow))
		fmt.Printf("  活动连接: %d\n", st.Conns)
		fmt.Printf("  运行时长: %s\n", st.Uptime)
		fmt.Printf("  运行目录: %s\n", st.Home)
		return nil
	}
	pid := readPID(p.PID)
	if pid > 0 && processAlive(pid) {
		fmt.Printf("状态: 运行中 (pid=%d)，但控制套接字不可用\n", pid)
		return nil
	}
	fmt.Println("状态: 未运行")
	return nil
}

// cmdInit 仅做初始化而不启动服务。
func cmdInit(o options) error {
	p := o.runtimePaths()
	if err := ensureInit(p); err != nil {
		return err
	}
	fmt.Printf("初始化完成: %s\n", p.Home)
	return nil
}

// cmdPasswd 设置 PASSWORD；参数为空则生成随机密码并打印。改密码即时生效，无需重启。
func cmdPasswd(o options, newPW string) error {
	p := o.runtimePaths()
	if err := ensureInit(p); err != nil {
		return err
	}
	generated := false
	if newPW == "" {
		newPW = genRandomPassword()
		if newPW == "" {
			return fmt.Errorf("随机密码生成失败")
		}
		generated = true
	}
	if err := setConfigValues(p.Conf, map[string]string{"PASSWORD": newPW}); err != nil {
		return err
	}
	_ = os.WriteFile(p.Password, []byte(newPW), 0o600)
	if generated {
		fmt.Printf("已生成新密码: %s\n", newPW)
	} else {
		fmt.Println("已设置新密码")
	}
	if strings.TrimSpace(loadConfig(p.Conf).Password) == "" {
		fmt.Println("提示: PASSWORD 为空表示已关闭密码登录，仅公钥可登录")
	} else {
		fmt.Println("提示: 改密码即时生效，无需重启")
	}
	return nil
}

// cmdShowPassword 打印当前密码。
func cmdShowPassword(o options) error {
	p := o.runtimePaths()
	if b, err := os.ReadFile(p.Password); err == nil && len(b) > 0 {
		fmt.Println(strings.TrimSpace(string(b)))
		return nil
	}
	pw := loadConfig(p.Conf).Password
	if pw == "" {
		fmt.Println("(密码登录已关闭)")
		return nil
	}
	fmt.Println(pw)
	return nil
}

// cmdSetUser / cmdSetPort / cmdSetAllow 修改对应配置项。
func cmdSetUser(o options, name string) error {
	p := o.runtimePaths()
	if name == "" {
		return fmt.Errorf("请提供用户名")
	}
	if err := setConfigValues(p.Conf, map[string]string{"USERNAME": name}); err != nil {
		return err
	}
	fmt.Printf("USERNAME=%s（即时生效）\n", name)
	return nil
}

func cmdSetPort(o options, port string) error {
	p := o.runtimePaths()
	if port == "" {
		return fmt.Errorf("请提供端口")
	}
	if _, err := strconv.Atoi(port); err != nil {
		return fmt.Errorf("无效端口: %s", port)
	}
	if err := setConfigValues(p.Conf, map[string]string{"SSH_PORT": port}); err != nil {
		return err
	}
	fmt.Printf("SSH_PORT=%s（需 restart 生效）\n", port)
	return nil
}

func cmdSetAllow(o options, nets string) error {
	p := o.runtimePaths()
	if _, err := parseAllowNets(nets); err != nil {
		return err
	}
	if err := setConfigValues(p.Conf, map[string]string{"ALLOW_NET": nets}); err != nil {
		return err
	}
	fmt.Printf("ALLOW_NET=%s（需 restart 生效）\n", orNone(nets))
	return nil
}

// cmdAddKey 追加一条公钥到 authorized_keys（校验格式）。
func cmdAddKey(o options, keyLine string) error {
	p := o.runtimePaths()
	if keyLine == "" {
		return fmt.Errorf("请提供公钥内容")
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(keyLine)); err != nil {
		return fmt.Errorf("公钥格式无效: %w", err)
	}
	f, err := os.OpenFile(p.AuthKeys, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(strings.TrimSpace(keyLine) + "\n"); err != nil {
		return err
	}
	fmt.Printf("已追加公钥: %s（即时生效）\n", p.AuthKeys)
	return nil
}

func readPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(不限制)"
	}
	return s
}
