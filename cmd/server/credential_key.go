package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// 加密密钥不写入数据库，默认只在私有数据目录生成一次，也可从环境变量提供。
func credentialKey(dataDir string) ([]byte, error) {
	if supplied := strings.TrimSpace(os.Getenv("RELAYPOOL_CREDENTIAL_KEY")); supplied != "" {
		return decodeCredentialKey(supplied)
	}
	path := filepath.Join(dataDir, "credential.key")
	if raw, err := os.ReadFile(path); err == nil {
		return decodeCredentialKey(strings.TrimSpace(string(raw)))
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("创建凭证密钥文件失败：%w", err)
	}
	_, writeErr := file.WriteString(base64.StdEncoding.EncodeToString(key))
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return key, nil
}

func decodeCredentialKey(text string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(text)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("凭证密钥须为 32 字节的标准 Base64 编码；请保留原密钥")
	}
	return key, nil
}
