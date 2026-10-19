package pool

import (
	"fmt"
	"testing"
)

// 用相同的模型目录和轮询请求，比较优化前后的热路径开销。
func BenchmarkPickRoute(b *testing.B) {
	for _, accounts := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("accounts_%d", accounts), func(b *testing.B) {
			p := New(StrategyRoundRobin)
			for index := accounts - 1; index >= 0; index-- {
				p.Upsert(item(fmt.Sprintf("acc_%04d", index), withModel("model-a", "model-b", "model-c", "model-d")))
			}
			query := RouteQuery{PublicModel: "model-d"}
			b.ReportAllocs()
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				if !p.PickRoute(query).OK {
					b.Fatal("测试账号未被选中")
				}
			}
		})
	}
}
