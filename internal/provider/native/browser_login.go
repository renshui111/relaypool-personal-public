package native

import (
	"context"
	"github.com/relaypool/relaypool/internal/provider"
	"github.com/relaypool/relaypool/internal/provider/browserlogin"
)

func (c *Client) BeginBrowserLogin(ctx context.Context, o provider.BrowserLoginOptions) (*provider.BrowserLoginSession, error) {
	return browserlogin.Begin(ctx, c.http, c.family, o)
}
func (c *Client) PollBrowserLogin(ctx context.Context, s *provider.BrowserLoginSession) ([]byte, bool, error) {
	if s.Family != c.family {
		return nil, false, provider.ErrUnsupported
	}
	return browserlogin.Poll(ctx, c.http, s)
}
func (c *Client) CompleteBrowserLogin(ctx context.Context, s *provider.BrowserLoginSession, callback string) ([]byte, error) {
	if s.Family != c.family {
		return nil, provider.ErrUnsupported
	}
	return browserlogin.Complete(ctx, c.http, s, callback)
}
