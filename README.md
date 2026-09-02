# ai-usage

Une commande, le pourcentage consommé de chaque abonnement IA.

```
$ ai-usage
Claude  pro  moi@exemple.com
  5h          ▓▓▓▓▓▓▓▓▓▓▓░░░░░  74%   reset 18:30 (4h12)
  7d          ▓░░░░░░░░░░░░░░░   8%   reset mer. 13:00 (3j22h)
  extra usage 0.00/1700.00 crédits

ChatGPT plus  moi@exemple.com
  weekly      ▓▓▓▓▓▓▓▓▓░░░░░░░  54%   reset mer. 13:28 (3j23h)

Grok  moi@exemple.com
  weekly      ▓▓░░░░░░░░░░░░░░  14%   reset mar. 21:36 (3j7h)

Kimi  basic
  5h          ▓░░░░░░░░░░░░░░░   8%   reset 18:04 (4h24)
  plan        ▓░░░░░░░░░░░░░░░   2%   reset dim. 13:04 (6j23h)

Copilot business  mon-login
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
ai-usage --short              une ligne : claude 74%/8% · chatgpt 54% · grok 14% · kimi 8%/2% · copilot 50%/∞/∞
ai-usage --json               sortie machine (schema_version 1)
ai-usage --offline            aucun appel réseau (voir plus bas)
ai-usage --only claude,grok   filtrer
ai-usage --check              diagnostic de dérive d'API
ai-usage renew                renouveler manuellement les tokens expirés
ai-usage --help               tous les flags
```

Codes de sortie : `0` tout va bien · `1` un provider en échec, une dérive détectée, ou
`--strict`/`--fail-over` déclenché · `2` erreur d'usage · `3` aucune donnée exploitable.

`--fail-over 80` sort en échec dès qu'une fenêtre dépasse 80 % — pratique en cron.

## Le risque principal : ces APIs ne sont pas documentées

Les cinq endpoints sont internes. Ils peuvent changer sans préavis, et le pire scénario
n'est pas la panne — c'est un **chiffre plausible mais faux** (un champ renommé, une échelle
qui passe de 0-100 à 0-1, « consommé » qui devient « restant »).

Le code est construit contre ça :

- **Aucun clamp silencieux.** Les providers stockent la valeur brute ; `Report.Validate`
  signale un pourcentage hors bornes, un reset dans le passé ou un reset à plus de 45 jours,
  *puis* borne pour l'affichage. Un avertissement `⚠` s'affiche toujours, jamais derrière un flag.
- **Périmé n'est pas dérive.** La plausibilité d'une date est jugée depuis l'instant où le
  chiffre a été produit (`fetched_at`), pas depuis maintenant : un cache local relu parce que
  le token a expiré est légitimement vieux de plusieurs heures, et sa fenêtre 5 h a réellement
  été franchie depuis. Ce cas affiche « fenêtre 5h réinitialisée depuis — ce pourcentage est
  obsolète » et sort en `0` ; l'alarme `⚠` de dérive reste réservée aux dates invraisemblables
  *au moment de la collecte*.
- **Décodage réussi ≠ données reconnues.** Si un endpoint répond 200 mais qu'aucun champ
  attendu n'est trouvé, le message le dit explicitement (« forme de réponse inattendue ») et
  le run sort en `1`, au lieu de retomber discrètement sur une donnée périmée.
- **Isolation.** Un provider cassé dégrade en ligne `n/a (raison)` ; il ne fait jamais tomber
  le rapport.
- **`ai-usage --check`** affiche, par provider, ce qui a réellement été reconnu (champs,
  durées, resets). C'est le premier réflexe quand un chiffre paraît faux.

### Forme observée des réponses (2026-07-26)

À comparer quand `--check` signale une dérive.

| Provider | Endpoint | Champs lus |
|---|---|---|
| Claude | `GET api.anthropic.com/api/oauth/usage` | `five_hour`/`seven_day`/`seven_day_{opus,sonnet}` → `{utilization 0-100, resets_at ISO}`, `extra_usage` |
| ChatGPT | `GET chatgpt.com/backend-api/wham/usage` | `rate_limit.{primary,secondary}_window.{used_percent, limit_window_seconds, reset_at epoch s}`, `plan_type`, `credits` |
| Grok | `GET cli-chat-proxy.grok.com/v1/billing?format=credits` | `config.creditUsagePercent` = % **consommé**, `config.currentPeriod.end` = reset ISO |
| Kimi | `GET api.kimi.com/coding/v1/usages` | `usage.{limit,used,remaining,resetTime}` (quota du plan), `limits[].{window.{duration,timeUnit},detail.{limit,used,remaining,resetTime}}`, `user.membership.level`, `user.userId` |
| Copilot | `GET api.github.com/copilot_internal/user` | `copilot_plan`, `login`, `quota_reset_date`, `quota_snapshots.{premium_interactions,chat,completions}.{percent_remaining, unlimited, overage_permitted}` |

ChatGPT écrit aussi ses snapshots dans `~/.codex/sessions/**/rollout-*.jsonl` sous une forme
**différente** (`rate_limits.primary.window_minutes`) : le parseur accepte les deux
orthographes, ne suppose jamais que `primary` vaut 5 h, et sélectionne par `limit_id`, jamais
par position.

## Credentials et renouvellement

**`ai-usage` ne lit, ne journalise et ne réécrit jamais un refresh token.**

Quand un access token Claude ou Codex est expiré, le binaire délègue son renouvellement au
CLI qui possède déjà le refresh token. Ce CLI peut alors mettre à jour son propre fichier de
credentials ; `ai-usage` se contente de relire l'access token pour vérifier le résultat.

Durées de vie constatées : Claude ~1 h, Grok ~6 h, Codex ~10 jours.

### Renouvellement automatique (renew)

Depuis la version avec `internal/renew`, `ai-usage` peut **déclencher** le rafraîchissement —
mais strictement en le **déléguant au CLI propriétaire du token**, jamais en le faisant lui-même :

- Dans le **chemin live** de la collecte (i.e. le cache est assez périmé pour justifier un refetch),
  si le token est **réellement expiré** (prédicat strict, sans la marge de sécurité de 60 s du
  rapport), `ai-usage` lance le CLI du fournisseur en mode print non-interactif avec un modèle bon
  marché. Cette invocation consomme tout de même une petite requête :
  - `claude -p "OK" --model haiku`
  - `codex exec "OK" -m luna --skip-git-repo-check`
- Le refresh OAuth se produit au **bootstrap du CLI**, avant tout appel modèle ; le petit prompt ne
  sert qu'à donner au CLI une raison d'atteindre ce bootstrap et de sortir tout seul.
- Le CLI utilise **sa propre identité et son propre refresh token** ; `ai-usage` relit ensuite le
  fichier pour confirmer que l'access token est redevenu utilisable. Il **ne touche jamais au
  refresh token** lui-même.
- Un verrou `flock` par provider (`~/.ai-usage/`) **sérialise les renouvellements entre processus** ;
  après l'acquisition, le token est relu avant tout lancement. Ce verrou est pris avec le même
  délai que le CLI. Sur une plateforme sans `flock` pris en charge, le renouvellement automatique
  refuse de lancer le CLI et la collecte suit sa politique de fallback normale.
- La cible est limitée au vrai fichier du CLI : pour Codex, uniquement `~/.codex/auth.json`, jamais
  le fallback Pi. Les overrides `CODEX_HOME` / `CLAUDE_CONFIG_DIR` et les tokens d'environnement
  sont retirés du child, et les deux CLIs démarrent depuis `~/.ai-usage/`, hors du projet courant.
- **Jamais** si le cache est frais (un cache hit ne relance jamais), **jamais** en `--offline`
  (statusline/prompt), **jamais** si le token n'est pas expiré.
- Si le refresh token est mort (session OAuth expirée côté serveur), le CLI ne peut pas rafraîchir :
  `ai-usage` le détecte (token toujours expiré après le run) et affiche **« please re-login »**
  avec la commande à lancer — le flux web OAuth est la seule étape qui exige l'humain.
- Le renouvellement automatique est best-effort : son échec est ajouté à la raison affichée,
  puis la collecte conserve la politique historique. Une donnée stale sort en `0` par défaut
  (`1` avec `--strict`) ; l'absence de toute donnée exploitable reste un échec.
- Les exécutables `claude` et `codex` sont résolus depuis le `PATH` de confiance de l'utilisateur.

Garde-fous de l'environnement inchangés :

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

TTL : Claude 180 s, ChatGPT 60 s, Grok 60 s, Copilot 60 s, OpenRouter 60 s, Kimi 300 s, OpenCode 300 s. `--refresh` ignore les TTL mais
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
rollouts Codex. C'est le mode à préférer pour une statusline ou un prompt shell. Grok, Kimi
et Copilot n'ont aucun cache local et se taisent alors.

## Sources de credentials

| Provider | Ordre de recherche |
|---|---|
| Claude | `~/.claude/.credentials.json` → `claudeAiOauth.accessToken` |
| ChatGPT | `~/.codex/auth.json` → `tokens.access_token`, puis `~/.pi/agent/auth.json` → `openai-codex` |
| Grok | `~/.grok/auth.json` → `<issuer>::<uuid>`.`key`, puis `~/.pi/agent/auth.json` → `xai` |
| Kimi | `~/.local/share/opencode/auth.json` → `kimi-for-coding`.`key` (ou `.access`), puis `$KIMI_API_KEY`/`$KIMI_CODE_API_KEY` |
| Copilot | `~/.config/github-copilot/{hosts,apps}.json`, puis `~/.local/share/opencode/auth.json`, puis `$GITHUB_TOKEN`/`$GH_TOKEN` |

## Quel compte ?

Chaque bloc nomme le compte auquel les chiffres appartiennent — un pourcentage sans
propriétaire n'est pas actionnable quand un Claude perso et un siège Copilot pro coexistent.

| Provider | Nom affiché | Id (`--verbose`) | Origine |
|---|---|---|---|
| Claude | email | `accountUuid` | `~/.claude.json → oauthAccount` |
| ChatGPT | email | `chatgpt_account_id` | claims du JWT (`profile`, `auth`) |
| Grok | email | `user_id` | `~/.grok/auth.json`, sinon `principal_id`/`sub` du JWT |
| Kimi | `userId` (faute de mieux) | `userId` | champ `user.userId` de la réponse `coding/v1/usages` |
| Copilot | `login` GitHub | — | champ `login` de la réponse `copilot_internal/user` |

Copilot est le seul à ne rien savoir hors ligne : les sources opencode et `$GITHUB_TOKEN` ne
contiennent qu'un token opaque, et le login vient de la réponse elle-même.

Kimi ne connaît **aucun nom** : la clé est un `sk-…` sans claims (pas un JWT), `coding/v1`
n'expose aucun endpoint de profil (`/me`, `/user`, `/users/me`, `/account`… tous en 404),
la clé est refusée par l'API plateforme `api.moonshot.ai`, et l'identité ne vit que derrière
le cookie navigateur `kimi-auth` de `www.kimi.com` — que cet outil ne lit pas. Faute de nom,
c'est l'identifiant opaque `userId` qui est affiché à sa place : un pourcentage sans
propriétaire n'est pas actionnable, un identifiant laid vaut mieux qu'un blanc.

`--verbose` ajoute l'id complet (ce qui distingue deux comptes partageant un email) et le
fichier de credentials retenu. `--json` expose `account` et `account_id`. `--short` reste une
seule ligne de pourcentages : aucun compte n'y apparaît.

## Alternative

[`openusage`](https://github.com/janekbaraniewski/openusage) couvre 35 providers. Il calcule
l'usage Claude depuis les transcripts locaux plutôt que depuis le % d'abonnement autoritatif,
et lit les rate limits xAI via `XAI_API_KEY` (pas l'usage d'un abonnement SuperGrok en OAuth) —
c'est précisément ce qui a motivé cet outil.
