package executor

import (
	"strconv"
	"testing"
	"time"
)

// Measure established conversations, excluding initial binding allocations.
// Rotate across entries so these include LRU updates, not just the head entry.
func BenchmarkSessionAffinity(b *testing.B) {
	for _, mode := range []string{"GetHit", "RefreshBinding", "GetAndBind"} {
		b.Run(mode, func(b *testing.B) {
			const count = 1024
			a := NewSessionAffinity(time.Hour, count)
			keys := make([]string, count)
			for i := range keys {
				keys[i] = "conversation-" + strconv.Itoa(i)
				a.Bind(keys[i], "account")
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				key := keys[i%count]
				if mode != "RefreshBinding" {
					if id, ok := a.Get(key); !ok || id != "account" {
						b.Fatal("active conversation lost its binding")
					}
				}
				if mode != "GetHit" {
					a.Bind(key, "account")
				}
			}
		})
	}
}
