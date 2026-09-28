package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

// DefaultHome 是运行态目录：配置、密钥、PID、控制套接字、日志均在此。
const DefaultHome = "/data/adb/android-sshd"

// Paths 描述一个实例的全部落盘路径，均由 home 派生，可被命令行覆盖。
type Paths struct {
	Home     string
	Conf     string
	AuthKeys string
	HostKey  string
	PID      string
	Sock     string
	Log      string
	Password string
}

func resolvePaths(home string) Paths {
	if home == "" {
		home = DefaultHome
	}
	return Paths{
		Home:     home,
		Conf:     filepath.Join(home, "sshd.conf"),
		AuthKeys: filepath.Join(home, "authorized_keys"),
		HostKey:  filepath.Join(home, "ssh_host_ed25519"),
		PID:      filepath.Join(home, "sshd.pid"),
		Sock:     filepath.Join(home, "sshd.sock"),
		Log:      filepath.Join(home, "sshd.log"),
		Password: filepath.Join(home, "password.txt"),
	}
}

// options 汇总命令行了全局标志，用于构建运行态路径与监听参数。
type options struct {
	home      string
	listen    string
	allow     string
	conf      string
	authKeys  string
	hostKey   string
	listenSet bool
	allowSet  bool
}

// runtimePaths 在 home 派生路径基础上应用显式覆盖（-config/-authorized-keys/-host-key）。
func (o options) runtimePaths() Paths {
	p := resolvePaths(o.home)
	if o.conf != "" {
		p.Conf = o.conf
	}
	if o.authKeys != "" {
		p.AuthKeys = o.authKeys
	}
	if o.hostKey != "" {
		p.HostKey = o.hostKey
	}
	return p
}

// Config 是 sshd.conf 的结构化表示。
type Config struct {
	Username string
	Password string
	Port     string
	AllowNet string
}

func defaultConfig() Config {
	return Config{Username: "root", Password: "", Port: "2222", AllowNet: "192.168.0.0/24"}
}

// loadConfig 读取 key=value 形式的配置文件（# 开头为注释），缺失项返回默认值。
// 每行去掉 \r，兼容 Windows 编辑。
func loadConfig(path string) Config {
	cfg := defaultConfig()
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
			cfg.Username = strings.TrimSpace(v)
		case "PASSWORD":
			cfg.Password = strings.TrimSpace(v)
		case "SSH_PORT":
			if p := strings.TrimSpace(v); p != "" {
				cfg.Port = p
			}
		case "ALLOW_NET":
			cfg.AllowNet = strings.TrimSpace(v)
		}
	}
	return cfg
}

// setConfigValues 以原子方式更新配置文件中若干 key 的值：已存在则替换，不存在则追加。
// 保留原有注释与顺序，权限 0600。
func setConfigValues(path string, kv map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	type entry struct {
		key, val string
	}
	var entries []entry
	seen := map[string]int{} // key -> index in entries

	if b, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(strings.ReplaceAll(line, "\r", ""))
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue // 丢弃空行，保留注释行需要原样写回
			}
			k, v, ok := strings.Cut(trimmed, "=")
			if !ok {
				continue
			}
			k = strings.TrimSpace(k)
			if idx, dup := seen[k]; dup {
				entries[idx].val = v
			} else {
				seen[k] = len(entries)
				entries = append(entries, entry{k, v})
			}
		}
	}

	// 应用更新
	for _, order := range []string{"USERNAME", "PASSWORD", "SSH_PORT", "ALLOW_NET"} {
		nv, ok := kv[order]
		if !ok {
			continue
		}
		if idx, exists := seen[order]; exists {
			entries[idx].val = nv
		} else {
			seen[order] = len(entries)
			entries = append(entries, entry{order, nv})
		}
	}

	var sb strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&sb, "%s=%s\n", e.key, e.val)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// genRandomPassword 生成 16 位十六进制随机口令。
func genRandomPassword() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
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

func portOf(listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		return listen[i+1:]
	}
	return listen
}
