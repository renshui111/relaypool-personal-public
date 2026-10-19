package main

// rawprobe dumps the raw upstream responses so the adapter's parsers can be
// corrected against reality instead of against a guess.
//
// It exists because two live failures (catalogue parse, empty non-stream text)
// can only be diagnosed by looking at exactly what the upstream sends.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/relaypool/relaypool/internal/provider/workbuddy"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: rawprobe <credential.json> <model>")
		os.Exit(2)
	}
	payload, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "read credential: %v\n", err)
		os.Exit(1)
	}
	model := os.Args[2]

	// ClientCredential returns this package's concrete credential type, whose
	// fields the header helpers read directly. The neutral provider.Credential
	// would not carry the domain, which decides the region.
	cred, err := workbuddy.ClientCredential(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "decode credential: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("domain=%s uid=%s region=%s\n\n", cred.Domain, cred.UID, cred.Region())

	// Resolve which host and path the adapter would use, then issue the request
	// with the adapter's own header set so the probe reproduces it exactly.
	chatBase := cred.ChatBase()
	catalogPath := "/console/enterprises/personal/models"
	if cred.IsGlobal() {
		catalogPath = "/v2/enterprises/personal/models"
	}

	proxyURL := os.Getenv("RELAYPOOL_PROXY_URL")
	transport := &http.Transport{}
	if proxyURL != "" {
		if parsed, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}
	httpClient := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	// --- catalogue ---
	fmt.Println("========== CATALOGUE ==========")
	endpoint := chatBase + catalogPath
	fmt.Printf("GET %s\n", endpoint)
	req, _ := http.NewRequest(http.MethodGet, endpoint, nil)
	workbuddy.SetCatalogHeaders(req.Header, cred)
	dumpResponse(httpClient, req)

	// --- chat (short) ---
	fmt.Println("\n========== CHAT (stream, raw first 3000 bytes) ==========")
	body := map[string]any{
		"model":  model,
		"stream": true,
		"messages": []any{
			map[string]any{"role": "system", "content": ""},
			map[string]any{"role": "user", "content": "Reply with exactly: OK"},
		},
		"max_tokens": 200,
	}
	encoded, _ := json.Marshal(body)
	chatReq, _ := http.NewRequest(http.MethodPost, chatBase+"/v2/chat/completions", bytes.NewReader(encoded))
	workbuddy.SetChatHeaders(chatReq.Header, cred)
	chatReq.Header.Set("Accept", "text/event-stream")
	dumpResponse(httpClient, chatReq)
}

func dumpResponse(client *http.Client, req *http.Request) {
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("ERROR: %v\n", err)
		return
	}
	defer resp.Body.Close()
	fmt.Printf("status: %d\n", resp.StatusCode)
	fmt.Printf("content-type: %s\n", resp.Header.Get("Content-Type"))
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4000))
	fmt.Printf("body (first 4000 bytes):\n%s\n", string(raw))
	if len(raw) > 0 {
		// Report the top-level JSON shape, which is what a parser needs to
		// match.
		var parsed map[string]any
		if json.Unmarshal(raw, &parsed) == nil {
			keys := make([]string, 0, len(parsed))
			for k := range parsed {
				keys = append(keys, k)
			}
			fmt.Printf("top-level JSON keys: %v\n", keys)
			for k, v := range parsed {
				switch typed := v.(type) {
				case []any:
					fmt.Printf("  %s: []any len=%d\n", k, len(typed))
					if len(typed) > 0 {
						if first, ok := typed[0].(map[string]any); ok {
							firstKeys := make([]string, 0, len(first))
							for fk := range first {
								firstKeys = append(firstKeys, fk)
							}
							fmt.Printf("    [0] keys: %v\n", firstKeys)
						}
					}
				case map[string]any:
					nested := make([]string, 0, len(typed))
					for nk := range typed {
						nested = append(nested, nk)
					}
					fmt.Printf("  %s: map keys=%v\n", k, nested)
				}
			}
		}
	}
}
