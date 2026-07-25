# ai-usage

Une commande, le pourcentage consommé de chaque abonnement IA.

```
$ ai-usage
Claude  pro
  5h          ▓▓▓▓▓▓▓▓▓▓▓░░░░░  74%   reset 18:30 (4h12)
  7d          ▓░░░░░░░░░░░░░░░   8%   reset mer. 13:00 (3j22h)
  extra usage 0.00/1700.00 crédits

ChatGPT plus
  weekly      ▓▓▓▓▓▓▓▓▓░░░░░░░  54%   reset mer. 13:28 (3j23h)

Grok
  window      ▓▓░░░░░░░░░░░░░░  14%   reset mar. 21:36 (3j7h)

Copilot business
  premium     ▓▓▓▓▓▓▓▓░░░░░░░░  50%   reset sam. 02:00 (6j11h)
  chat        illimité
  complétions illimité
```

Aucune configuration : les credentials déjà écrits dans `$HOME` par `claude`, `codex`, `pi`,
`grok` et `opencode` sont découverts automatiquement.

## Installation

```sh
make install     # go build + symlink dans ~/.local/bin/ai-usage
```

Go 1.26+, zéro dépendance, binaire statique.

## Usage

```
ai-usage                      rapport complet
ai-usage --short              une ligne : claude 74%/8% · chatgpt 54% · grok 14% · copilot 50%/∞/∞
ai-usage --json               sortie machine (schema_version 1)
ai-usage --offline            aucun appel réseau (voir plus bas)
ai-usage --only claude,grok   filtrer
ai-usage --check              diagnostic de dérive d'API
ai-usage --help               tous les flags
```

Codes de sortie : `0` tout va bien · `1` un provider en échec, une dérive détectée, ou
`--strict`/`--fail-over` déclenché · `2` erreur d'usage · `3` aucune donnée exploitable.

`--fail-over 80` sort en échec dès qu'une fenêtre dépasse 80 % — pratique en cron.

## Le risque principal : ces APIs ne sont pas documentées

Les quatre endpoints sont internes. Ils peuvent changer sans préavis, et le pire scénario
n'est pas la panne — c'est un **chiffre plausible mais faux** (un champ renommé, une échelle
qui passe de 0-100 à 0-1, « consommé » qui devient « restant »).

Le code est construit contre ça :

- **Aucun clamp silencieux.** Les providers stockent la valeur brute ; `Report.Validate`
  signale un pourcentage hors bornes, un reset dans le passé ou un reset à plus de 45 jours,
  *puis* borne pour l'affichage. Un avertissement `⚠` s'affiche toujours, jamais derrière un flag.
- **Décodage réussi ≠ données reconnues.** Si un endpoint répond 200 mais qu'aucun champ
  attendu n'est trouvé, le message le dit explicitement (« forme de réponse inattendue ») et
  le run sort en `1`, au lieu de retomber discrètement sur une donnée périmée.
- **Isolation.** Un provider cassé dégrade en ligne `n/a (raison)` ; il ne fait jamais tomber
  le rapport.
- **`ai-usage --check`** affiche, par provider, ce qui a réellement été reconnu (champs,
  durées, resets). C'est le premier réflexe quand un chiffre paraît faux.

### Forme observée des réponses (2026-07-25)

À comparer quand `--check` signale une dérive.

| Provider | Endpoint | Champs lus |
|---|---|---|
| Claude | `GET api.anthropic.com/api/oauth/usage` | `five_hour`/`seven_day`/`seven_day_{opus,sonnet}` → `{utilization 0-100, resets_at ISO}`, `extra_usage` |
| ChatGPT | `GET chatgpt.com/backend-api/wham/usage` | `rate_limit.{primary,secondary}_window.{used_percent, limit_window_seconds, reset_at epoch s}`, `plan_type`, `credits` |
| Grok | `POST grok.com/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig` | gRPC-Web ; enveloppe champ 1 → `CreditsConfig` ; champ 1 float32 LE = % **consommé**, champ 5 = Timestamp du reset |
| Copilot | `GET api.github.com/copilot_internal/user` | `copilot_plan`, `quota_reset_date`, `quota_snapshots.{premium_interactions,chat,completions}.{percent_remaining, unlimited, overage_permitted}` |

ChatGPT écrit aussi ses snapshots dans `~/.codex/sessions/**/rollout-*.jsonl` sous une forme
**différente** (`rate_limits.primary.window_minutes`) : le parseur accepte les deux
orthographes, ne suppose jamais que `primary` vaut 5 h, et sélectionne par `limit_id`, jamais
par position.

## Lecture seule, strictement

**Aucun token n'est rafraîchi, réécrit ou journalisé.**

Rafraîchir un token OAuth fait tourner le refresh token côté serveur, ce qui invaliderait la
copie détenue par `claude` / `codex` / `pi` / `grok` — et te déconnecterait de ton outil de
travail. À l'expiration, `ai-usage` dégrade et dit quoi lancer (`claude`, `codex login`,
`pi` puis `/login xai`).

Durées de vie constatées : Claude ~1 h, Grok ~6 h, Codex ~10 jours.

Autres garde-fous :

- Le fichier `~/.codex/auth.json` contient un `id_token` qui peut être **expiré de plusieurs
  jours** alors que l'`access_token` reste valide. La fraîcheur est lue dans le claim `exp` de
  l'`access_token` seul ; `id_token` et `last_refresh` sont ignorés.
- Tout corps de réponse en erreur passe par `Redact()` avant d'atteindre le terminal.
- Le cache (`$XDG_CACHE_HOME/ai-usage/usage.json`, `0600`, répertoire `0700`, écriture
  atomique) ne contient que des rapports normalisés — jamais un token, jamais une réponse
  brute. L'invalidation par changement de compte utilise une empreinte `sha256` tronquée.
- `main()` supprime `OPENAI_BASE_URL`, `*_API_KEY` et les variables de proxy avant la première
  requête. Sans ça, un `OPENAI_BASE_URL` pointant sur un proxy local détournerait
  silencieusement l'appel ChatGPT. Les hôtes sont codés en dur.

## Politique d'appel

Le cache n'est pas un confort : c'est ce qui applique la recommandation Anthropic de ≥ 180 s
entre deux appels. Ce rate limit est **rattaché à l'access token, donc partagé avec ton vrai
client Claude Code** — poller agressivement throttlerait ton travail réel.

TTL : Claude 180 s, ChatGPT 60 s, Grok 60 s, Copilot 300 s. `--refresh` ignore les TTL mais
**conserve** le plancher Anthropic ; seul `--force` l'outrepasse. Pas de retry : sur `429` on
sert la donnée en cache immédiatement.

Les appels Claude, ChatGPT et Grok envoient le `User-Agent` du client officiel — côté
Anthropic c'est obligatoire, tout autre UA se fait 429 en permanence. La version envoyée est
toujours celle **réellement installée** (résolue via le symlink `~/.local/bin/claude`), jamais
une version inventée. Lire son propre usage n'est pas du contournement de limite, mais se
présenter comme un autre client reste une zone grise vis-à-vis de la politique Anthropic
« Authentication and credential use ».

**`--offline` est la version sans aucune de ces réserves** : zéro réseau, zéro usurpation. Il
relit `~/.claude.json → cachedUsageUtilization` (que Claude Code rafraîchit lui-même) et les
rollouts Codex. C'est le mode à préférer pour une statusline ou un prompt shell. Grok et
Copilot n'ont aucun cache local et se taisent alors.

## Sources de credentials

| Provider | Ordre de recherche |
|---|---|
| Claude | `~/.claude/.credentials.json` → `claudeAiOauth.accessToken` |
| ChatGPT | `~/.codex/auth.json` → `tokens.access_token`, puis `~/.pi/agent/auth.json` → `openai-codex` |
| Grok | `~/.grok/auth.json` → `<issuer>::<uuid>`.`key`, puis `~/.pi/agent/auth.json` → `xai` |
| Copilot | `~/.config/github-copilot/{hosts,apps}.json`, puis `~/.local/share/opencode/auth.json`, puis `$GITHUB_TOKEN`/`$GH_TOKEN` |

`--verbose` affiche le fichier retenu et le compte (email) ; ces informations n'apparaissent
jamais sans ce flag, ni dans `--short`.

## Crédits

Le décodage de l'usage SuperGrok (endpoint, framing gRPC-Web, numéros de champs protobuf) est
porté de `~/.pi/agent/extensions/xai-supergrok-usage.ts`.

## Alternative

[`openusage`](https://github.com/janekbaraniewski/openusage) couvre 35 providers. Il calcule
l'usage Claude depuis les transcripts locaux plutôt que depuis le % d'abonnement autoritatif,
et lit les rate limits xAI via `XAI_API_KEY` (pas l'usage d'un abonnement SuperGrok en OAuth) —
c'est précisément ce qui a motivé cet outil.
