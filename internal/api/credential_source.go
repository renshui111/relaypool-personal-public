package api

import (
	"context"
	"fmt"
	"time"

	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/store"
)

// credentialLoadTimeout bounds a credential read. It is short because the load
// happens on the request path and the read is a single indexed row lookup.
const credentialLoadTimeout = 3 * time.Second

// credentialSource adapts the store to the executor's CredentialSource.
//
// The decode step deliberately goes through the ADAPTER rather than parsing the
// blob here: each provider owns its own credential schema, and the store must
// be able to round-trip a blob it does not understand. Centralizing the parse
// in the gateway would mean every new channel required a gateway change.
type credentialSource struct {
	store       *store.Store
	registry    *provider.Registry
	OnRefreshed func(string)
}

// CredentialFor loads and decodes an account's credential.
func (c credentialSource) CredentialFor(accountID string) (provider.Credential, error) {
	// 定时保活自行刷新，读取时不再刷新一次，避免同一令牌连续轮换。
	return c.loadCredential(context.Background(), accountID, false)
}

// 请求中的刷新继承客户端取消信号，不让断开的请求继续等待上游。
func (c credentialSource) CredentialForContext(parent context.Context, accountID string) (provider.Credential, error) {
	return c.loadCredential(parent, accountID, true)
}

func (c credentialSource) loadCredential(parent context.Context, accountID string, refresh bool) (provider.Credential, error) {
	if c.store == nil {
		return provider.Credential{}, errString("no credential store configured")
	}
	// The pool holds no credential data, so a short timeout here cannot stall
	// scheduling; the read is a single indexed row lookup.
	ctx, cancel := context.WithTimeout(parent, credentialLoadTimeout)
	defer cancel()

	// 账号路由字段随凭证一次读取，不在每个请求里反序列化完整健康历史。
	record, err := c.store.GetCredential(ctx, accountID)
	if err != nil {
		return provider.Credential{}, err
	}
	adapter, err := c.registry.Lookup(record.Provider)
	if err != nil {
		return provider.Credential{}, err
	}
	cred, err := adapter.DecodeCredential(record.Payload)
	if err != nil {
		return provider.Credential{}, err
	}
	cred.AccountID = accountID
	if cred.Region != "" && string(cred.Region) != record.Region {
		return provider.Credential{}, fmt.Errorf("凭证地区与账号配置不一致")
	}
	// Keep the raw blob: the adapter may re-decode it, and the executor uses
	// Raw as the "is there anything here" signal.
	cred.Raw = record.Payload
	if cred.Provider == "" {
		cred.Provider = record.Provider
	}
	needsRefresh := (cred.Provider == "claude" || cred.Provider == "gemini" || cred.Provider == "codex") && !cred.ExpiresAt.IsZero() && time.Until(cred.ExpiresAt) < time.Minute && cred.Field("refresh_token") != ""
	if policy, ok := adapter.(provider.RefreshPolicy); ok {
		needsRefresh = policy.NeedsRefresh(cred)
	}
	if refresh && needsRefresh {
		refreshCtx, stop := context.WithTimeout(parent, 30*time.Second)
		defer stop()
		updated, err := adapter.Refresh(refreshCtx, cred)
		if err != nil {
			return provider.Credential{}, err
		}
		saved, err := c.store.ReplaceCredentialIfUnchanged(refreshCtx, record.Payload, store.Credential{AccountID: accountID, Format: record.Format, Payload: updated.Raw, ExpiresAt: updated.ExpiresAt})
		if err != nil {
			return provider.Credential{}, err
		}
		if saved {
			if c.OnRefreshed != nil {
				c.OnRefreshed(accountID)
			}
			return updated, nil
		}
		// 用户在刷新过程中替换了凭证，使用数据库里的新值，禁止覆盖。
		latest, err := c.store.GetCredential(refreshCtx, accountID)
		if err != nil {
			return provider.Credential{}, err
		}
		cred, err = adapter.DecodeCredential(latest.Payload)
		if err != nil {
			return provider.Credential{}, err
		}
		cred.AccountID = accountID
	}
	return cred, nil
}
