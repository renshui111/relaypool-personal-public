package api

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/store"
)

// 使用权限上限附近的真实选号过滤路径，衡量白名单增长的成本。
func BenchmarkPermissionRouting(b *testing.B) {
	s := &Server{pool: pool.New(pool.StrategyRoundRobin), registry: provider.NewRegistry()}
	p := store.KeyPermissions{}
	for i := range 1000 {
		id := fmt.Sprintf("account-%04d", i)
		s.pool.Upsert(pool.Item{ID: id, Provider: "workbuddy"})
		p.AccountIDs = append(p.AccountIDs, id)
		p.Models = append(p.Models, fmt.Sprintf("workbuddy/model-%04d", i))
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r = r.WithContext(context.WithValue(r.Context(), keyPermissionsContextKey{}, store.APIKey{Permissions: p}))
	w := httptest.NewRecorder()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ids, ok := s.permittedRouteAccounts(w, r, "model-0999", "workbuddy")
		if !ok || len(ids) != 1000 {
			b.Fatal("基准测试未完成真实权限过滤")
		}
	}
}
