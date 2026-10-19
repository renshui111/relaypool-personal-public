package main

// modelprobe tests ONE model against the live upstream and prints the raw
// outcome. It exists to answer "is this model usable at all?" for a specific
// model ID, which a full livecheck run makes tedious to isolate.
//
// Usage: modelprobe <credential.json> <model> [prompt]

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/workbuddy"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: modelprobe <credential.json> <model> [prompt]")
		os.Exit(2)
	}
	credPath, model := os.Args[1], os.Args[2]
	prompt := "Reply with exactly: OK"
	if len(os.Args) > 3 {
		prompt = os.Args[3]
	}

	payload, err := os.ReadFile(credPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read credential: %v\n", err)
		os.Exit(1)
	}
	cred, err := workbuddy.ClientCredential(payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "decode credential: %v\n", err)
		os.Exit(1)
	}

	client := workbuddy.New(workbuddy.Config{
		Timeout:  60 * time.Second,
		ProxyURL: os.Getenv("RELAYPOOL_PROXY_URL"),
	})

	// 1. Is the model even advertised by this account?
	ctx := context.Background()
	catCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	models, catErr := client.FetchCatalog(catCtx, credField(cred))
	cancel()
	advertised := false
	if catErr == nil {
		for _, m := range models {
			if m.NativeModel == model {
				advertised = true
				break
			}
		}
	}
	fmt.Printf("model                : %s\n", model)
	fmt.Printf("catalogue fetch      : %v\n", errText(catErr))
	fmt.Printf("advertised by account: %v (%d models in catalogue)\n", advertised, len(models))
	fmt.Println()

	// 2. A minimal completion, with the smallest possible message list.
	req := &provider.ChatRequest{
		Model: model,
		Messages: []provider.Message{
			{Role: "user", Content: json.RawMessage(`"` + prompt + `"`)},
		},
		MaxTokens: intPtr(128),
	}

	chatCtx, cancel2 := context.WithTimeout(ctx, 90*time.Second)
	defer cancel2()
	result, err := client.ChatNonStream(chatCtx, credField(cred), req)
	if err != nil {
		fmt.Printf("RESULT: FAILED\n")
		fmt.Printf("  %s\n", errText(err))
		if failure, ok := provider.AsFailure(err); ok {
			fmt.Printf("  status=%d code=%s kind=%s\n", failure.Status, failure.Code, failure.KindHint)
			fmt.Printf("  body=%s\n", truncate(failure.Body, 900))
		}
		os.Exit(1)
	}
	var text string
	_ = json.Unmarshal(result.Message.Content, &text)
	fmt.Printf("RESULT: OK\n")
	fmt.Printf("  reply=%q\n", truncate(text, 200))
	fmt.Printf("  usage: prompt=%d completion=%d\n", result.Usage.PromptTokens, result.Usage.CompletionTokens)
}

// credField adapts the concrete credential to the neutral type the client
// methods take.
func credField(c workbuddy.Credential) provider.Credential {
	raw, _ := c.Encode()
	return provider.Credential{
		Provider: workbuddy.Family,
		Region:   c.Region(),
		Raw:      raw,
		Fields: map[string]string{
			"access_token":  c.AccessToken,
			"refresh_token": c.RefreshToken,
			"uid":           c.UID,
			"enterprise_id": c.EnterpriseID,
			"domain":        c.Domain,
		},
	}
}

func intPtr(v int) *int { return &v }

func errText(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

var _ = io.Discard
