package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestCredentialRefreshCannotOverwriteReplacement(t *testing.T) {
	ctx := context.Background()
	s, err := OpenWithCredentialKey(ctx, filepath.Join(t.TempDir(), "test.db"), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	account, err := s.CreateAccount(ctx, Account{Name: "test", Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	old := []byte(`{"access_token":"old"}`)
	newValue := []byte(`{"access_token":"user-new"}`)
	if err := s.UpsertCredential(ctx, Credential{AccountID: account.ID, Format: "test", Payload: old}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertCredential(ctx, Credential{AccountID: account.ID, Format: "test", Payload: newValue}); err != nil {
		t.Fatal(err)
	}
	updated, err := s.ReplaceCredentialIfUnchanged(ctx, old, Credential{AccountID: account.ID, Format: "test", Payload: []byte(`{"access_token":"stale-refresh"}`)})
	if err != nil || updated {
		t.Fatal("旧刷新结果覆盖新凭证")
	}
	current, err := s.GetCredential(ctx, account.ID)
	if err != nil || string(current.Payload) != string(newValue) {
		t.Fatal("凭证变化")
	}
	updated, err = s.ReplaceCredentialIfUnchanged(ctx, newValue, Credential{AccountID: account.ID, Format: "test", Payload: []byte(`{"access_token":"fresh"}`)})
	if err != nil || !updated {
		t.Fatal("合法刷新失败")
	}
	current, _ = s.GetCredential(ctx, account.ID)
	if string(current.Payload) != `{"access_token":"fresh"}` {
		t.Fatal("加密更新无法还原")
	}
}
