package main

// livecheck verifies the WorkBuddy adapter against the REAL upstream using
// credentials exported from a working deployment.
//
// This is the highest-value test in the project: unit tests prove the code is
// self-consistent, but only a live call proves the protocol constants, header
// set, and payload shape actually match what the upstream accepts.
//
// Usage: livecheck <accounts.json> <credential-dir>
//
// It is a separate command rather than a _test.go file on purpose, so the
// normal `go test ./...` never reaches the network.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/workbuddy"
)

type accountRow struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Region   string `json:"provider_region"`
	Enabled  int    `json:"enabled"`
}

// results accumulates a pass/fail summary.
type results struct {
	passed int
	failed int
}

func (r *results) check(label string, ok bool, detail string) {
	if ok {
		fmt.Printf("  PASS  %s\n", label)
		r.passed++
		return
	}
	fmt.Printf("  FAIL  %s\n", label)
	if detail != "" {
		fmt.Printf("        %s\n", detail)
	}
	r.failed++
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: livecheck <accounts.json> <credential-dir>")
		os.Exit(2)
	}
	accountsPath := os.Args[1]
	credDir := os.Args[2]

	rawAccounts, err := os.ReadFile(accountsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read accounts: %v\n", err)
		os.Exit(1)
	}
	var accounts []accountRow
	if err := json.Unmarshal(rawAccounts, &accounts); err != nil {
		fmt.Fprintf(os.Stderr, "parse accounts: %v\n", err)
		os.Exit(1)
	}

	// The adapter is configured exactly as the server configures it, including
	// the proxy. Without the proxy the container cannot resolve the upstream.
	proxyURL := os.Getenv("RELAYPOOL_PROXY_URL")
	client := workbuddy.New(workbuddy.Config{
		Timeout:       60 * time.Second,
		HeaderTimeout: 60 * time.Second,
		IdleTimeout:   120 * time.Second,
		ProxyURL:      proxyURL,
	})
	fmt.Printf("adapter: family=%s kind=%s proxy=%q\n\n", client.Family(), client.Kind(), proxyURL)

	var res results
	ctx := context.Background()

	for _, account := range accounts {
		credPath := filepath.Join(credDir, account.ID+".json")
		payload, err := os.ReadFile(credPath)
		if err != nil {
			fmt.Printf("=== %s (%s/%s) SKIP: no credential file\n\n", account.ID, account.Provider, account.Region)
			continue
		}
		enabled := account.Enabled != 0
		fmt.Printf("=== %s  name=%q provider=%s region=%s enabled=%v\n",
			account.ID, account.Name, account.Provider, account.Region, enabled)

		// 1. Decode the stored blob with the adapter's own decoder. This proves
		//    the export format is one the adapter understands.
		cred, err := client.DecodeCredential(payload)
		res.check("credential decodes", err == nil, fmt.Sprintf("%v", err))
		if err != nil {
			fmt.Println()
			continue
		}
		region := cred.Field("domain")
		res.check("region detected from domain", cred.Region == provider.RegionCN || cred.Region == provider.RegionGlobal,
			fmt.Sprintf("domain=%q region=%q", region, cred.Region))
		res.check("credential is ready (token + uid)", cred.Ready(),
			fmt.Sprintf("token=%d chars uid=%q", len(cred.Field("access_token")), cred.Field("uid")))
		if !cred.ExpiresAt.IsZero() {
			remaining := time.Until(cred.ExpiresAt).Round(time.Hour)
			fmt.Printf("        expires in %s (%s)\n", remaining, cred.ExpiresAt.Format(time.RFC3339))
		}

		if !enabled {
			// Disabled accounts are still decoded to prove the format, but not
			// called: the operator disabled them for a reason.
			fmt.Printf("        (disabled, skipping live calls)\n\n")
			continue
		}

		// 2. Fetch the model catalogue. This exercises the region-specific
		//    path, the identity headers, and the catalogue envelope parser.
		fmt.Println("  -- FetchCatalog (live)")
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		models, err := client.FetchCatalog(callCtx, cred)
		cancel()
		res.check("catalogue fetch succeeds", err == nil, errText(err))
		if err == nil {
			res.check("catalogue is non-empty", len(models) > 0, fmt.Sprintf("%d models", len(models)))
			names := make([]string, 0, len(models))
			for i, m := range models {
				if i >= 8 {
					names = append(names, "...")
					break
				}
				names = append(names, m.NativeModel)
			}
			fmt.Printf("        models: %v\n", names)
		}

		// 3. A real non-streaming completion. This is the decisive test: it
		//    proves the chat path, the CLI channel headers, the forced-stream
		//    rewrite, and the local SSE aggregation all work against the
		//    upstream, not just against a fixture.
		fmt.Println("  -- ChatNonStream (live)")
		model := pickModel(models)
		fmt.Printf("        using model %q\n", model)
		chatCtx, cancel2 := context.WithTimeout(ctx, 90*time.Second)
		result, err := client.ChatNonStream(chatCtx, cred, &provider.ChatRequest{
			Model: model,
			Messages: []provider.Message{
				{Role: "user", Content: json.RawMessage(`"Reply with exactly: OK"`)},
			},
			MaxTokens: intPtr(32),
		})
		cancel2()
		res.check("non-streaming completion succeeds", err == nil, errText(err))
		if err == nil && result != nil {
			var text string
			_ = json.Unmarshal(result.Message.Content, &text)
			fmt.Printf("        reply: %q\n", truncate(text, 120))
			fmt.Printf("        usage: prompt=%d completion=%d total=%d\n",
				result.Usage.PromptTokens, result.Usage.CompletionTokens, result.Usage.TotalTokens)
			res.check("completion returned text", text != "", fmt.Sprintf("raw=%s", string(result.Message.Content)))
		}

		// 4. A real streaming completion, verifying the sanitizer produces a
		//    clean stream with no blank frames.
		fmt.Println("  -- ChatStream (live)")
		streamCtx, cancel3 := context.WithTimeout(ctx, 90*time.Second)
		stream, err := client.ChatStream(streamCtx, cred, &provider.ChatRequest{
			Model: model,
			Messages: []provider.Message{
				{Role: "user", Content: json.RawMessage(`"Count from 1 to 3."`)},
			},
			MaxTokens: intPtr(48),
			Stream:    true,
		})
		res.check("streaming call opens", err == nil, errText(err))
		if err == nil && stream != nil {
			body := readAllCapped(stream.Body, 1<<20)
			stream.Close()
			cancel3()
			res.check("stream contains [DONE]", contains(body, "[DONE]"), truncate(body, 200))
			res.check("stream contains no blank content field",
				!contains(body, `"content":""`), "a blank frame survived the sanitizer")
			res.check("stream contains no blank reasoning field",
				!contains(body, `"reasoning_content":""`), "a blank reasoning frame survived")
			fmt.Printf("        frames: %d bytes\n", len(body))
			fmt.Printf("        head: %s\n", truncate(oneline(body), 220))
		} else {
			cancel3()
		}

		fmt.Println()
	}

	fmt.Printf("=== %d passed, %d failed ===\n", res.passed, res.failed)
	if res.failed > 0 {
		os.Exit(1)
	}
}

// pickModel chooses a model for the chat call, preferring a small fast one.
func pickModel(models []provider.ModelInfo) string {
	preferred := []string{"deepseek-v4.1-flash", "glm-5.2", "deepseek-v4-flash"}
	for _, want := range preferred {
		for _, m := range models {
			if m.NativeModel == want {
				return want
			}
		}
	}
	if len(models) > 0 {
		return models[0].NativeModel
	}
	return "auto"
}

func intPtr(v int) *int { return &v }

func errText(err error) string {
	if err == nil {
		return ""
	}
	// A provider.Failure carries the upstream status and body, which is the
	// useful part for diagnosing a protocol mismatch.
	if failure, ok := provider.AsFailure(err); ok {
		return fmt.Sprintf("status=%d code=%s msg=%s body=%s",
			failure.Status, failure.Code, failure.Message, truncate(failure.Body, 300))
	}
	return err.Error()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func oneline(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' {
			out = append(out, '|')
			continue
		}
		out = append(out, r)
	}
	return string(out)
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func readAllCapped(r interface{ Read([]byte) (int, error) }, max int) string {
	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 8192)
	for len(buf) < max {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return string(buf)
}
