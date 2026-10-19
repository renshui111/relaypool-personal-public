package main

// catalogdump prints the exact JSON shape of the upstream catalogue response so
// the parser can be written against reality.
//
// The live run revealed the response nests the model list differently than
// assumed, and a parser written from an assumption silently rejects a 200 OK.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"time"

	"github.com/relaypool/relaypool/internal/provider/workbuddy"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: catalogdump <credential.json>")
		os.Exit(2)
	}
	payload, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "read credential: %v\n", err)
		os.Exit(1)
	}
	cred, err := workbuddy.ClientCredential(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "decode credential: %v\n", err)
		os.Exit(1)
	}

	catalogPath := "/console/enterprises/personal/models"
	if cred.IsGlobal() {
		catalogPath = "/v2/enterprises/personal/models"
	}
	endpoint := cred.ChatBase() + catalogPath

	transport := &http.Transport{}
	if proxyURL := os.Getenv("RELAYPOOL_PROXY_URL"); proxyURL != "" {
		if parsed, err := url.Parse(proxyURL); err == nil {
			transport.Proxy = http.ProxyURL(parsed)
		}
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}

	req, _ := http.NewRequest(http.MethodGet, endpoint, nil)
	workbuddy.SetCatalogHeaders(req.Header, cred)
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "request: %v\n", err)
		os.Exit(1)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	fmt.Printf("status=%d bytes=%d\n\n", resp.StatusCode, len(raw))

	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		fmt.Fprintf(os.Stderr, "not a JSON object: %v\n", err)
		os.Exit(1)
	}
	describe("", parsed, 0)
}

// describe prints the shape of a decoded JSON value, expanding one level of
// arrays and maps so nested structures are visible without dumping every value.
func describe(path string, value any, depth int) {
	indent := ""
	for i := 0; i < depth; i++ {
		indent += "  "
	}
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for k := range typed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := typed[k]
			switch child.(type) {
			case map[string]any, []any:
				fmt.Printf("%s%s.%s:\n", indent, path, k)
				describe(path+"."+k, child, depth+1)
			default:
				preview := fmt.Sprintf("%v", child)
				if len(preview) > 60 {
					preview = preview[:60] + "..."
				}
				fmt.Printf("%s%s.%s = %s\n", indent, path, k, preview)
			}
		}
	case []any:
		fmt.Printf("%s%s[]: len=%d\n", indent, path, len(typed))
		if len(typed) > 0 {
			// Only the first element: sibling elements share a shape.
			describe(path+"[0]", typed[0], depth+1)
		}
	default:
		fmt.Printf("%s%s = %v\n", indent, path, typed)
	}
}
