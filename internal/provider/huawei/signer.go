package huawei

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
)

func hash(data []byte) string   { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func uriEncode(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }

// SDK-HMAC-SHA256: canonical URI has a trailing slash; canonical headers
// have their own final newline. No Authorization header participates in itself.
func sign(r *http.Request, body []byte, cred provider.Credential) error {
	if cred.Field("access_key") == "" || cred.Field("secret_key") == "" {
		return fmt.Errorf("华为签名缺少 AK/SK")
	}
	r.Header.Del("Authorization")
	if r.Header.Get("X-Sdk-Date") == "" {
		r.Header.Set("X-Sdk-Date", time.Now().UTC().Format("20060102T150405Z"))
	}
	if v := cred.Field("security_token"); v != "" {
		r.Header.Set("X-Security-Token", v)
	}
	if v := cred.Field("project_id"); v != "" {
		r.Header.Set("X-Project-ID", v)
	}
	headers := map[string]string{"host": r.URL.Host}
	for k, v := range r.Header {
		headers[strings.ToLower(k)] = strings.TrimSpace(strings.Join(v, ","))
	}
	keys := []string{}
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var canonical strings.Builder
	for _, k := range keys {
		canonical.WriteString(k + ":" + headers[k] + "\n")
	}
	parts := strings.Split(r.URL.EscapedPath(), "/")
	for i, part := range parts {
		decoded, err := url.PathUnescape(part)
		if err != nil {
			return fmt.Errorf("华为签名路径无效")
		}
		parts[i] = uriEncode(decoded)
	}
	uri := strings.Join(parts, "/")
	if !strings.HasSuffix(uri, "/") {
		uri += "/"
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return fmt.Errorf("华为签名参数无效")
	}
	qkeys := []string{}
	for k := range query {
		qkeys = append(qkeys, k)
	}
	sort.Strings(qkeys)
	pairs := []string{}
	for _, k := range qkeys {
		values := query[k]
		sort.Strings(values)
		for _, v := range values {
			pairs = append(pairs, uriEncode(k)+"="+uriEncode(v))
		}
	}
	payload := body
	if r.Method != "POST" && r.Method != "PUT" && r.Method != "PATCH" {
		payload = nil
	}
	signedHeaders := strings.Join(keys, ";")
	request := strings.Join([]string{r.Method, uri, strings.Join(pairs, "&"), canonical.String(), signedHeaders, hash(payload)}, "\n")
	toSign := "SDK-HMAC-SHA256\n" + r.Header.Get("X-Sdk-Date") + "\n" + hash([]byte(request))
	mac := hmac.New(sha256.New, []byte(cred.Field("secret_key")))
	mac.Write([]byte(toSign))
	r.Header.Set("Authorization", "SDK-HMAC-SHA256 Access="+cred.Field("access_key")+", SignedHeaders="+signedHeaders+", Signature="+hex.EncodeToString(mac.Sum(nil)))
	return nil
}
