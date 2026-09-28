package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"log"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

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
