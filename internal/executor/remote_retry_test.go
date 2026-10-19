package executor

import (
	"github.com/relaypool/relaypool/internal/pool"
	"github.com/relaypool/relaypool/internal/provider"
	"testing"
)

func TestRemoteCommittedFailureDisablesOnlyFailover(t *testing.T) {
	e := &Executor{opts: Options{}}
	for _, committed := range []bool{false, true} {
		classified := e.classify(pool.Item{}, Request{Chat: &provider.ChatRequest{Model: "m"}}, &provider.Failure{Status: 503, NoRetry: committed})
		if classified.Failover == committed || classified.Kind != pool.KindUnavailable {
			t.Fatal("任务受理标记破坏分类或允许重复创建")
		}
	}
}
