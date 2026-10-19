package api

import (
	"testing"

	"github.com/relaypool/relaypool/internal/pool"
)

// 状态回调可能乱序到达，旧版本不能覆盖新版本，即使新版本已经写盘。
func TestOptimizationStateWriterOrdersSnapshots(t *testing.T) {
	st := openTestStore(t)
	defer st.Close()
	ctx := t.Context()
	if _, err := st.CreateAccount(ctx, accountFixture("acc_order")); err != nil {
		t.Fatal(err)
	}
	w := newStateWriter(st)
	latest := pool.Item{ID: "acc_order", StateVersion: 20, LastError: "最新状态", LastKind: "auth"}
	old := pool.Item{ID: "acc_order", StateVersion: 10, LastError: "旧状态", LastKind: "unavailable"}
	w.observe(latest)
	w.observe(old)
	w.flush(ctx)
	checkLatest := func() {
		t.Helper()
		account, err := st.GetAccount(ctx, latest.ID)
		if err != nil {
			t.Fatal(err)
		}
		if account.LastError != latest.LastError {
			t.Fatalf("旧快照覆盖了新状态：%s", account.LastError)
		}
	}
	checkLatest()
	w.observe(old)
	w.flush(ctx)
	checkLatest()
}
