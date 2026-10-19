package subscriptions

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/relaypool/relaypool/internal/provider"
)

// COSY 编码及签名依据 MIT 项目 cpa-multi-plugins 固定提交改写，许可见本目录。
// 这是服务器公开密钥，用于包装每次会话密钥，不是账号秘密。
const qoderPublicKey = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

type cosySession struct{ key, wrapped, info, machineID, machineToken, machineType string }

func qoderEncode(plain []byte) string {
	const standard = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="
	const custom = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!$"
	encoded := base64.StdEncoding.EncodeToString(plain)
	n, a := len(encoded), len(encoded)/3
	rearranged := encoded[n-a:] + encoded[a:n-a] + encoded[:a]
	output := make([]byte, len(rearranged))
	for i, char := range []byte(rearranged) {
		output[i] = custom[strings.IndexByte(standard, char)]
	}
	return string(output)
}

func newCosySession(cred provider.Credential) (*cosySession, error) {
	block, _ := pem.Decode([]byte(qoderPublicKey))
	if block == nil {
		return nil, fmt.Errorf("Qoder 公开密钥无效")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	public, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("Qoder 公开密钥类型无效")
	}
	key := strings.ReplaceAll(uuid.NewString(), "-", "")[:16]
	wrapped, err := rsa.EncryptPKCS1v15(rand.Reader, public, []byte(key))
	if err != nil {
		return nil, err
	}
	identity := map[string]string{"name": cred.Field("nickname"), "aid": cred.Field("uid"), "uid": cred.Field("uid"), "yx_uid": "", "organization_id": "", "organization_name": "", "user_type": "personal_professional_trial", "security_oauth_token": cred.Field("access_token"), "refresh_token": cred.Field("refresh_token")}
	raw, _ := json.Marshal(identity)
	pad := aes.BlockSize - len(raw)%aes.BlockSize
	for i := 0; i < pad; i++ {
		raw = append(raw, byte(pad))
	}
	aesBlock, err := aes.NewCipher([]byte(key))
	if err != nil {
		return nil, err
	}
	encrypted := make([]byte, len(raw))
	cipher.NewCBCEncrypter(aesBlock, []byte(key)).CryptBlocks(encrypted, raw)
	// 原客户端设备字段优先；未提供时使用账号稳定标识，不随换令牌变化。
	machineID := cred.Field("machine_id")
	if machineID == "" {
		machineID = uuid.NewSHA1(uuid.NameSpaceURL, []byte("relaypool/qoder/"+string(cred.Region)+"/"+cred.AccountID+"/"+cred.Field("uid"))).String()
	}
	machineToken := cred.Field("machine_token")
	if machineToken == "" {
		seed := uuid.NewSHA1(uuid.NameSpaceURL, []byte(machineID)).String()
		machineToken = base64.RawURLEncoding.EncodeToString([]byte(seed + seed)[:50])
	}
	machineType := cred.Field("machine_type")
	if machineType == "" {
		machineType = strings.ReplaceAll(machineID, "-", "")
		if len(machineType) > 18 {
			machineType = machineType[:18]
		}
	}
	return &cosySession{key: key, wrapped: base64.StdEncoding.EncodeToString(wrapped), info: base64.StdEncoding.EncodeToString(encrypted), machineID: machineID, machineToken: machineToken, machineType: machineType}, nil
}

func (c *Client) qoderSession(cred provider.Credential) (*cosySession, error) {
	key := credentialKey(cred)
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.sessions {
		if now.Sub(v.used) > 5*time.Minute {
			delete(c.sessions, k)
		}
	}
	if entry, ok := c.sessions[key]; ok {
		entry.used = now
		c.sessions[key] = entry
		return entry.session, nil
	}
	if len(c.sessions) >= 128 {
		var oldest string
		var when time.Time
		for k, v := range c.sessions {
			if oldest == "" || v.used.Before(when) {
				oldest, when = k, v.used
			}
		}
		delete(c.sessions, oldest)
	}
	session, err := newCosySession(cred)
	if err == nil {
		c.sessions[key] = sessionEntry{session: session, used: now}
	}
	return session, err
}

func (c *Client) qoderHeaders(cred provider.Credential, endpoint string, body []byte, model string) (http.Header, error) {
	session, err := c.qoderSession(cred)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]string{"cosyVersion": "0.1.43", "ideVersion": "", "info": session.info, "requestId": uuid.NewString(), "version": "v1"})
	encoded := base64.StdEncoding.EncodeToString(payload)
	date := fmt.Sprint(time.Now().Unix())
	// 上游协议使用 MD5；只用于协议互操作，账号落盘仍由现有加密存储负责。
	signature := md5.Sum([]byte(encoded + "\n" + session.wrapped + "\n" + date + "\n" + string(body) + "\n" + strings.TrimPrefix(u.Path, "/algo")))
	h := http.Header{}
	for k, v := range map[string]string{"Cosy-Data-Policy": "AGREE", "Content-Type": "application/json", "Cosy-Machinetype": session.machineType, "Cosy-Clienttype": "5", "Cosy-Date": date, "Cosy-User": cred.Field("uid"), "Cosy-Key": session.wrapped, "Cosy-Version": "0.1.43", "Cosy-Machineid": session.machineID, "Cosy-Machinetoken": session.machineToken, "Login-Version": "v2", "User-Agent": "Go-http-client/2.0", "Accept-Encoding": "identity", "Authorization": "Bearer COSY." + encoded + "." + hex.EncodeToString(signature[:])} {
		h.Set(k, v)
	}
	if model != "" {
		h.Set("Accept", "text/event-stream")
		h.Set("X-Model-Key", model)
		h.Set("X-Model-Source", "system")
	} else {
		h.Set("Accept", "application/json")
	}
	return h, nil
}

func (c *Client) qoderCatalog(ctx context.Context, cred provider.Credential) ([]provider.ModelInfo, error) {
	endpoint := cred.Field("base_url") + qoderModelsPath
	body := []byte(qoderEncode([]byte("{}")))
	headers, err := c.qoderHeaders(cred, endpoint, body, "")
	if err != nil {
		return nil, err
	}
	response, err := c.do(ctx, cred, "GET", endpoint, body, headers, false)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var listing struct {
		Chat []struct {
			Key       string `json:"key"`
			Name      string `json:"display_name"`
			Enable    bool   `json:"enable"`
			Reasoning bool   `json:"is_reasoning"`
			Images    bool   `json:"is_vl"`
			Context   int    `json:"max_input_tokens"`
		} `json:"chat"`
	}
	if err := decodeLimited(response.Body, &listing); err != nil {
		return nil, err
	}
	out := []provider.ModelInfo{}
	for _, model := range listing.Chat {
		if model.Enable && model.Key != "" {
			out = append(out, provider.ModelInfo{NativeModel: model.Key, PublicModel: model.Key, DisplayName: model.Name, ContextWindow: model.Context, Tools: true, Images: model.Images, Reasoning: model.Reasoning})
		}
	}
	if len(out) == 0 {
		return nil, failure(502, "Qoder 返回的聊天型号目录为空")
	}
	return out, nil
}
