package main

// payloadprobe sends a request through the adapter's FULL outbound pipeline
// (PrepareBody included) so the exact bytes the upstream receives can be
// inspected.
//
// It exists because a bare HTTP probe bypasses PrepareBody, and a difference
// there -- the inserted empty system message in particular -- is a real
// candidate for an "empty content" rejection.
//
// Usage: payloadprobe <credential.json> <model>

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/workbuddy"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: payloadprobe <credential.json> <model>")
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
	model := os.Args[2]
	// An optional third argument selects the payload shape to send. This is the
	// control in the experiment: the same request with and without the empty
	// system message that PrepareBody inserts.
	shape := "prepared"
	if len(os.Args) > 3 {
		shape = os.Args[3]
	}

	client := workbuddy.New(workbuddy.Config{
		Timeout:  60 * time.Second,
		ProxyURL: os.Getenv("RELAYPOOL_PROXY_URL"),
	})

	// Build the outgoing body for the requested shape.
	raw := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"Reply with exactly: OK"}],"max_tokens":128}`)
	var outgoing []byte
	switch shape {
	case "prepared":
		// The real gateway path: PrepareBody runs, which inserts an empty
		// system message for models that require a leading system turn.
		outgoing = workbuddy.PrepareBody(raw)
	case "bare":
		// The control: no system message at all.
		outgoing = raw
	case "nonempty-system":
		// A second control: a system message that actually has content.
		outgoing = []byte(`{"model":"` + model + `","messages":[{"role":"system","content":"You are a helpful assistant."},{"role":"user","content":"Reply with exactly: OK"}],"max_tokens":128}`)
	default:
		fmt.Fprintf(os.Stderr, "unknown shape %q (want prepared|bare|nonempty-system)\n", shape)
		os.Exit(2)
	}

	fmt.Printf("=== shape=%s: payload the upstream will receive ===\n", shape)
	var pretty map[string]any
	if err := json.Unmarshal(outgoing, &pretty); err == nil {
		out, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Println(string(out))
	} else {
		fmt.Println(string(outgoing))
	}
	fmt.Println()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Send it through the real request path.
	req := &provider.ChatRequest{
		Model: model,
		Messages: []provider.Message{
			{Role: "user", Content: json.RawMessage(`"Reply with exactly: OK"`)},
		},
		MaxTokens: intPtr(128),
		Raw:       outgoing,
	}
	result, err := client.ChatNonStream(ctx, credField(cred), req)
	if err != nil {
		fmt.Printf("RESULT: FAILED\n  %s\n", err.Error())
		if failure, ok := provider.AsFailure(err); ok {
			fmt.Printf("  status=%d code=%s\n", failure.Status, failure.Code)
		}
		os.Exit(1)
	}
	var text string
	_ = json.Unmarshal(result.Message.Content, &text)
	fmt.Printf("RESULT: OK  reply=%q\n", text)
}

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
