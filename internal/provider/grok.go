package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/valche5/ai-usage/internal/credstore"
	"github.com/valche5/ai-usage/internal/httpx"
)

// grokUsageURL is the JSON endpoint used by the Grok CLI's billing display.
const grokUsageURL = "https://cli-chat-proxy.grok.com/v1/billing?format=credits"

// Grok reports SuperGrok subscription utilization.
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
		ID:        g.ID(),
		Name:      g.Name(),
		CredPath:  credstore.Display(cred.Path),
		Account:   cred.Email,
		AccountID: cred.AccountID,
		TokenFP:   credstore.Fingerprint(cred.Token),
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
		Method: http.MethodGet,
		URL:    grokUsageURL,
		Headers: map[string]string{
			"Authorization":    "Bearer " + cred.Token,
			"X-XAI-Token-Auth": "xai-grok-cli",
			"Accept":           "application/json",
		},
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

	credits, err := parseGrokBilling(res.Body)
	if err != nil {
		base.Status = StatusError
		base.Reason = driftReason("réponse illisible : " + err.Error())
		return base
	}

	base.Status = StatusOK
	base.Source = SourceLive
	base.FetchedAt = o.Now
	base.Windows = []Window{{
		Key:         "weekly",
		Label:       "weekly",
		UsedPercent: credits.UsagePercent,
		ResetsAt:    credits.ResetAt,
	}}
	return base
}

type grokBillingResponse struct {
	Config *struct {
		CurrentPeriod *struct {
			Type  string `json:"type"`
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"currentPeriod"`
		CreditUsagePercent float64 `json:"creditUsagePercent"`
		BillingPeriodEnd   string  `json:"billingPeriodEnd"`
	} `json:"config"`
}

type grokCredits struct {
	UsagePercent float64
	ResetAt      *time.Time
}

// parseGrokBilling decodes the JSON contract used by Grok CLI 1.0.13.
// Protobuf JSON omits scalar zero values, so a missing creditUsagePercent in
// an otherwise valid config is the legitimate representation of 0% usage.
func parseGrokBilling(raw []byte) (grokCredits, error) {
	var response grokBillingResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return grokCredits{}, fmt.Errorf("JSON invalide: %w", err)
	}
	if response.Config == nil {
		return grokCredits{}, errors.New("champ config absent")
	}
	if math.IsNaN(response.Config.CreditUsagePercent) || math.IsInf(response.Config.CreditUsagePercent, 0) {
		return grokCredits{}, errors.New("creditUsagePercent non numérique")
	}

	end := response.Config.BillingPeriodEnd
	if response.Config.CurrentPeriod != nil && response.Config.CurrentPeriod.End != "" {
		end = response.Config.CurrentPeriod.End
	}
	var resetAt *time.Time
	if end != "" {
		t, err := time.Parse(time.RFC3339Nano, end)
		if err != nil {
			return grokCredits{}, fmt.Errorf("date de reset invalide: %w", err)
		}
		resetAt = &t
	}

	return grokCredits{
		UsagePercent: response.Config.CreditUsagePercent,
		ResetAt:      resetAt,
	}, nil
}
