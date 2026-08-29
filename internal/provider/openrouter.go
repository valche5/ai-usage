package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/valche5/ai-usage/internal/credstore"
	"github.com/valche5/ai-usage/internal/httpx"
)

// openrouterCreditsURL reports the account-wide prepaid pool: total credits
// bought and how many have been spent. Hardcoded like every other host here:
// no env var may redirect it.
const openrouterCreditsURL = "https://openrouter.ai/api/v1/credits"

// OpenRouter reports consumption of the prepaid credits pool, which is
// account-wide: the numbers come back identical for every key of the account.
type OpenRouter struct{}

func (OpenRouter) ID() string   { return "openrouter" }
func (OpenRouter) Name() string { return "OpenRouter" }

func (OpenRouter) Fingerprint(now time.Time) string {
	cands, err := credstore.OpenRouter()
	if err != nil {
		return ""
	}
	cred, _ := credstore.Choose(cands, now)
	return credstore.Fingerprint(cred.Token)
}

type openRouterCreditsResponse struct {
	Data struct {
		TotalCredits float64 `json:"total_credits"`
		TotalUsage   float64 `json:"total_usage"`
	} `json:"data"`
}

func (o OpenRouter) Collect(ctx context.Context, opts Options) Report {
	cands, err := credstore.OpenRouter()
	switch {
	case errors.Is(err, credstore.ErrMissing):
		return Unconfigured(o.ID(), o.Name(),
			"pas de clé OpenRouter (entrée `openrouter` dans l'auth pi ou opencode)")
	case err != nil:
		return Errorf(o.ID(), o.Name(), err.Error())
	}

	cred, _ := credstore.Choose(cands, opts.Now)
	base := Report{
		ID:       o.ID(),
		Name:     o.Name(),
		CredPath: credstore.Display(cred.Path),
		TokenFP:  credstore.Fingerprint(cred.Token),
	}

	// OpenRouter exposes no local usage cache, so this provider goes quiet
	// offline rather than showing a number it cannot date.
	if opts.Offline {
		base.Status = StatusError
		base.Reason = "mode hors ligne — OpenRouter n'a aucun cache local"
		return base
	}

	var resp openRouterCreditsResponse
	_, err = httpx.JSON(ctx, opts.HTTP, httpx.Req{
		URL: openrouterCreditsURL,
		Headers: map[string]string{
			"Authorization": "Bearer " + cred.Token,
			"Accept":        "application/json",
		},
	}, &resp)
	if err != nil {
		base.Status = StatusError
		base.Reason = err.Error()
		if httpx.IsUnauthorized(err) {
			base.Reason = "clé OpenRouter refusée — regénère la clé sur openrouter.ai/keys"
		}
		return base
	}

	total, used := resp.Data.TotalCredits, resp.Data.TotalUsage
	if total <= 0 {
		base.Status = StatusError
		base.Reason = driftReason("aucun total_credits dans la réponse")
		return base
	}

	base.Status = StatusOK
	base.Source = SourceLive
	base.FetchedAt = opts.Now
	base.Windows = []Window{{
		Key:         "solde",
		Label:       "solde",
		UsedPercent: used / total * 100,
	}}
	base.Notes = []string{fmt.Sprintf("solde : $%.2f restants sur $%.2f", total-used, total)}
	return base
}
