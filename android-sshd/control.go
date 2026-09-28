package main

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

// statusReply 是控制套接字对 STATUS 命令的应答。
type statusReply struct {
	Running  bool   `json:"running"`
	PID      int    `json:"pid"`
	Listen   string `json:"listen"`
	Allow    string `json:"allow"`
	Username string `json:"username"`
	Conns    int64  `json:"conns"`
	Uptime   string `json:"uptime"`
	Home     string `json:"home"`
}

// serveControl 在 Unix 域套接字上处理管理命令：PING / STATUS / STOP / RELOAD。
// 协议为单行命令 + 一行 JSON 应答。仅本机 root 进程可访问（socket 权限 0600）。
func (s *server) serveControl() {
	for {
		c, err := s.ctl.Accept()
		if err != nil {
			select {
			case <-s.stopCh:
				return
			default:
				log.Printf("控制套接字接受失败: %v", err)
				continue
			}
		}
		go s.handleControlConn(c)
	}
}

func (s *server) handleControlConn(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	line, _, err := bufio.NewReader(c).ReadLine()
	if err != nil {
		return
	}
	cmd := strings.ToUpper(strings.TrimSpace(string(line)))
	switch cmd {
	case "PING":
		s.writeControl(c, map[string]any{"ok": true, "pong": true})
	case "STATUS":
		s.mu.Lock()
		conns := int64(len(s.conns))
		s.mu.Unlock()
		_ = os.Chmod(s.paths.Sock, 0o600)
		s.writeControl(c, statusReply{
			Running:  true,
			PID:      os.Getpid(),
			Listen:   s.listen,
			Allow:    s.cfg.AllowNet,
			Username: loadConfig(s.paths.Conf).Username,
			Conns:    conns,
			Uptime:   time.Since(s.started).Round(time.Second).String(),
			Home:     s.paths.Home,
		})
	case "STOP":
		s.writeControl(c, map[string]any{"ok": true, "stopping": true})
		go func() { time.Sleep(50 * time.Millisecond); s.shutdown() }()
	case "RELOAD":
		s.writeControl(c, map[string]any{"ok": true, "note": "配置与公钥本就每次登录热加载，无需重启"})
	default:
		s.writeControl(c, map[string]any{"ok": false, "error": "未知命令: " + cmd})
	}
}

func (s *server) writeControl(c net.Conn, v any) {
	b, _ := json.Marshal(v)
	_, _ = c.Write(append(b, '\n'))
}

// controlRequest 向指定套接字发送命令并返回原始 JSON 应答。
func controlRequest(sock, cmd string) ([]byte, error) {
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(cmd + "\n")); err != nil {
		return nil, err
	}
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, err
	}
	return line, nil
}

// pingSock 判断是否有实例在控制套接字上响应。
func pingSock(sock string) bool {
	if sock == "" {
		return false
	}
	if _, err := os.Stat(sock); err != nil {
		return false
	}
	_, err := controlRequest(sock, "PING")
	return err == nil
}

// queryStatus 拉取运行态信息；无响应返回 false。
func queryStatus(sock string) (statusReply, bool) {
	var st statusReply
	b, err := controlRequest(sock, "STATUS")
	if err != nil {
		return st, false
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, false
	}
	return st, true
}
