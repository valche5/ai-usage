package provider

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/valche5/ai-usage/internal/credstore"
	"github.com/valche5/ai-usage/internal/httpx"
)

// opencodeUsageURL is the Go usage endpoint of the OpenCode zen gateway.
// Hardcoded like every other host here: no env var may redirect it.
const opencodeUsageURL = "https://opencode.ai/zen/go/v1/usage"

// OpenCode reports OpenCode Go subscription utilization.
type OpenCode struct{}

func (OpenCode) ID() string   { return "opencode" }
func (OpenCode) Name() string { return "OpenCode" }

func (OpenCode) Fingerprint(now time.Time) string {
	cands, err := credstore.OpenCode()
	if err != nil {
		return ""
	}
	cred, _ := credstore.Choose(cands, now)
	return credstore.Fingerprint(cred.Token)
}

// opencodeUsageWindow is one Go quota window: a percentage plus when it resets.
type opencodeUsageWindow struct {
	Status   string  `json:"status"`
	Percent  float64 `json:"percent"`
	ResetsAt *string `json:"resetsAt"`
}

type opencodeUsageResponse struct {
	Usage struct {
		Rolling *opencodeUsageWindow `json:"rolling"`
		Weekly  *opencodeUsageWindow `json:"weekly"`
		Monthly *opencodeUsageWindow `json:"monthly"`
	} `json:"usage"`
}

func (o OpenCode) Collect(ctx context.Context, opts Options) Report {
	cands, err := credstore.OpenCode()
	switch {
	case errors.Is(err, credstore.ErrMissing):
		return Unconfigured(o.ID(), o.Name(),
			"pas de clé OpenCode (entrée `opencode` ou `opencode-go` dans l'auth opencode)")
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

	// The zen gateway exposes no local usage cache, so this provider goes
	// quiet offline rather than showing a number it cannot date.
	if opts.Offline {
		base.Status = StatusError
		base.Reason = "mode hors ligne — OpenCode n'a aucun cache local"
		return base
	}

	var resp opencodeUsageResponse
	_, err = httpx.JSON(ctx, opts.HTTP, httpx.Req{
		URL: opencodeUsageURL,
		Headers: map[string]string{
			"Authorization": "Bearer " + cred.Token,
			"Accept":        "application/json",
		},
	}, &resp)
	if err != nil {
		base.Status = StatusError

		// A 403 here is not a bad key: the gateway answers "Go subscription
		// required" for a workspace that lives on the zen balance instead,
		// which the API does not expose at all.
		var he httpx.HTTPError
		if errors.As(err, &he) && he.Status == 403 && strings.Contains(he.Excerpt, "Go subscription") {
			base.Reason = "pas d'abonnement OpenCode Go — la balance zen n'est pas exposée via l'API (voir opencode.ai/workspace)"
			return base
		}
		base.Reason = err.Error()
		if httpx.IsUnauthorized(err) {
			base.Reason = "clé OpenCode refusée — regénère la clé sur opencode.ai/workspace"
		}
		return base
	}

	var windows []Window
	add := func(label string, mins int, w *opencodeUsageWindow) {
		if w == nil {
			return
		}
		windows = append(windows, Window{
			Key:           label,
			Label:         label,
			UsedPercent:   w.Percent,
			ResetsAt:      parseISO(w.ResetsAt),
			WindowMinutes: mins,
		})
	}
	// The rolling window is the Go plan's 5-hour bucket; monthly has no fixed
	// length, so it is labelled for what it is rather than given invented
	// minutes.
	add("5h", 300, resp.Usage.Rolling)
	add("weekly", 10080, resp.Usage.Weekly)
	add("monthly", 0, resp.Usage.Monthly)

	if len(windows) == 0 {
		base.Status = StatusError
		base.Reason = driftReason("aucun quota exploitable dans usage")
		return base
	}

	base.Status = StatusOK
	base.Source = SourceLive
	base.FetchedAt = opts.Now
	base.Windows = windows
	base.Plan = "go"
	return base
}
