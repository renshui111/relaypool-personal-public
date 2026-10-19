package store

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"strings"
)

const secretPrefix = "relaypool:secret:v1:"

func (s *Store) encryptSecret(name, value string) (string, error) {
	if s.credentialCipher == nil {
		return value, nil
	}
	sealed, err := s.encryptCredential("app-secret:"+name, []byte(value))
	if err != nil {
		return "", err
	}
	return secretPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

func (s *Store) decryptSecret(name, value string) (string, error) {
	if !strings.HasPrefix(value, secretPrefix) {
		return value, nil
	}
	if s.credentialCipher == nil {
		return "", fmt.Errorf("读取加密秘密需要原加密密钥")
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, secretPrefix))
	if err != nil || !bytes.HasPrefix(data, credentialPrefix) {
		return "", fmt.Errorf("加密秘密编码无效")
	}
	plain, err := s.decryptCredential("app-secret:"+name, data)
	if err != nil {
		return "", fmt.Errorf("秘密解密失败，请核对原加密密钥")
	}
	return string(plain), nil
}
