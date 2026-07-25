package provider

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/valche5/ai-usage/internal/credstore"
	"github.com/valche5/ai-usage/internal/grpcweb"
	"github.com/valche5/ai-usage/internal/httpx"
)

// grokUsageURL is the Connect/gRPC-Web method behind grok.com/?_s=usage.
const grokUsageURL = "https://grok.com/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig"

// Grok reports SuperGrok subscription utilization.
//
// The endpoint, framing and header set come from the working reverse
// engineering already in ~/.pi/agent/extensions/xai-supergrok-usage.ts.
type Grok struct{}

func (Grok) ID() string   { return "grok" }
func (Grok) Name() string { return "Grok" }

func (Grok) Fingerprint(now time.Time) string {
	cands, err := credstore.Grok()
	if err != nil {
		return ""
	}
	cred, _ := credstore.Choose(cands, now)
	return credstore.Fingerprint(cred.Token)
}

func (g Grok) Collect(ctx context.Context, o Options) Report {
	cands, err := credstore.Grok()
	switch {
	case errors.Is(err, credstore.ErrMissing):
		return Unconfigured(g.ID(), g.Name(),
			"pas de session xAI (~/.grok/auth.json ou entrée `xai` dans ~/.pi/agent/auth.json)")
	case err != nil:
		return Errorf(g.ID(), g.Name(), err.Error())
	}

	cred, valid := credstore.Choose(cands, o.Now)
	base := Report{
		ID:       g.ID(),
		Name:     g.Name(),
		CredPath: credstore.Display(cred.Path),
		Account:  cred.Email,
		TokenFP:  credstore.Fingerprint(cred.Token),
	}

	// xAI exposes no local usage cache, so this provider simply goes quiet
	// offline rather than showing a stale number.
	if o.Offline {
		base.Status = StatusError
		base.Reason = "mode hors ligne — xAI n'a aucun cache local"
		return base
	}
	if !valid {
		base.Status = StatusError
		base.Reason = "session xAI expirée — lance `grok` ou `pi` puis /login xai"
		return base
	}

	res, err := httpx.Do(ctx, o.HTTP, httpx.Req{
		Method: http.MethodPost,
		URL:    grokUsageURL,
		Headers: map[string]string{
			"authorization": "Bearer " + cred.Token,
			"content-type":  "application/grpc-web+proto",
			"x-grpc-web":    "1",
			"x-user-agent":  "connect-es/2.1.1",
			"origin":        "https://grok.com",
			"referer":       "https://grok.com/?_s=usage",
			"accept":        "*/*",
		},
		Body: grpcweb.EmptyMessage,
	})
	if err != nil {
		base.Status = StatusError
		base.Reason = err.Error()
		return base
	}
	if res.Status < 200 || res.Status > 299 {
		he := httpx.HTTPError{Status: res.Status, Excerpt: res.Excerpt}
		base.Status = StatusError
		base.Reason = he.Error()
		if httpx.IsUnauthorized(he) {
			base.Reason = "session xAI refusée — lance `grok` ou `pi` puis /login xai"
		}
		return base
	}

	credits, err := grpcweb.ParseCreditsConfig(res.Body)
	if err != nil {
		base.Status = StatusError
		base.Reason = driftReason("réponse illisible : " + err.Error())
		return base
	}

	base.Status = StatusOK
	base.Source = SourceLive
	base.FetchedAt = o.Now
	base.Windows = []Window{{
		Key:         "window",
		Label:       "window",
		UsedPercent: credits.UsagePercent,
		ResetsAt:    credits.ResetAt,
	}}
	return base
}
