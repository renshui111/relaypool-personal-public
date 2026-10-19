package scheduler

import (
	"context"
	"github.com/relaypool/relaypool/internal/provider"
	"testing"
	"time"
)

// 启动后立即运行清理，停机取消正在运行的任务；零保留值不启动任务。
func TestLogRetentionLifecycle(t *testing.T) {
	for _, days := range []int{0, 7} {
		t.Run(string(rune('0'+days)), func(t *testing.T) {
			s := New(provider.NewRegistry(), nil, nil, Options{RetentionDays: days})
			called := make(chan time.Duration, 1)
			ended := make(chan struct{})
			s.SetLogPruner(func(ctx context.Context, age time.Duration) (int64, error) {
				called <- age
				<-ctx.Done()
				close(ended)
				return 0, ctx.Err()
			})
			s.Start(t.Context())
			if days == 0 {
				select {
				case <-called:
					t.Fatal("关闭清理仍运行")
				case <-time.After(30 * time.Millisecond):
				}
				s.Stop()
				return
			}
			select {
			case age := <-called:
				if age != 7*24*time.Hour {
					t.Fatal("未传入配置保留期")
				}
			case <-time.After(time.Second):
				t.Fatal("启动没有清理")
			}
			s.Stop()
			select {
			case <-ended:
			default:
				t.Fatal("停机后清理任务仍在运行")
			}
		})
	}
}
