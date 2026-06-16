# projet-etude-app-demo

Portail de tickets (tracker d'incidents) en Go, rendu HTML via HTMX. Trois binaires sous `cmd/` : `api` (serveur HTTP, port 8080), `worker` (jobs asynchrones via LISTEN/NOTIFY Postgres, métriques sur 8081), `audit-purge` (job one-shot de purge RGPD de l'`audit_log`).

## Prérequis

- Go 1.25
- Un PostgreSQL joignable via `DATABASE_URL` pour exécuter l'application
- Docker, uniquement pour les tests d'intégration (voir plus bas)

## Build

```bash
# les trois binaires
go build ./...

# ou un par un
go build -o bin/api          ./cmd/api
go build -o bin/worker       ./cmd/worker
go build -o bin/audit-purge  ./cmd/audit-purge
```

L'image conteneur est une distroless statique (`CGO_ENABLED=0`), avec trois cibles `api`, `worker`, `audit-purge`. Voir le `Dockerfile`.

## Tests

```bash
go test ./...
go test -race ./...   # comme en CI
```

Les tests d'intégration utilisent testcontainers-go : ils démarrent un Postgres `16-alpine` éphémère. Un démon Docker doit donc tourner localement, sinon ces tests échouent au lancement du conteneur. Sur un devShell Nix sans Docker natif, passer par le démon Docker host ou `colima`/`lima`.

## Variables d'environnement

| Variable | Composant | Rôle | Défaut |
| --- | --- | --- | --- |
| `DATABASE_URL` | api, worker, audit-purge | DSN Postgres | `postgres://app:app@localhost:5432/app?sslmode=disable` pour api et worker ; requis (pas de défaut) pour audit-purge |
| `METRICS_ADDR` | worker | adresse d'écoute des métriques | `:8081` |
| `AUDIT_RETENTION_DAYS` | audit-purge | rétention de l'audit log en jours | `90` |
| `DEMO_CHAOS_ENABLED` | api | active les endpoints chaos (`/api/panic`, `/api/crash`, `/api/memleak`, `/api/flaky`, `/api/slow`) si la valeur vaut `1` | désactivé |
| `WEBHOOK_URL` | worker | URL du webhook sortant | vide |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | api, worker | collecteur OTLP gRPC ; vide signifie aucune trace exportée | vide |
| `OTEL_TRACES_SAMPLER` | api, worker | `always_on`, `always_off` ou ratio | `always_on` |
| `APP_VERSION` | api, worker | version reportée dans les traces | vide |

Authentification OIDC, lues par `internal/auth` :

| Variable | Rôle |
| --- | --- |
| `OIDC_ISSUER_URL` | issuer OIDC (Dex en cible) |
| `OIDC_CLIENT_ID` | client ID |
| `OIDC_CLIENT_SECRET` | client secret |
| `OIDC_REDIRECT_URL` | URL de redirection après login |
| `OIDC_VIEWER_GROUP` | groupe mappé sur le rôle viewer (défaut `viewers`) |
| `OIDC_OPERATOR_GROUP` | groupe mappé sur le rôle operator (défaut `operators`) |
| `OIDC_OPERATOR_EMAILS` | emails operator séparés par des virgules (fallback sans groupes) |
| `SESSION_KEY` | clé HMAC des sessions web |

## Authentification OIDC

OIDC s'active dès que `OIDC_ISSUER_URL` et `OIDC_CLIENT_ID` sont tous les deux renseignés (`internal/auth/auth.go`). Tant que l'un manque, l'authentification reste désactivée et chaque requête passe en `operator` (mode démo). Pour activer l'auth réelle, renseigner au minimum `OIDC_ISSUER_URL` et `OIDC_CLIENT_ID`, plus `OIDC_CLIENT_SECRET`, `OIDC_REDIRECT_URL` et `SESSION_KEY`, en pointant vers un connecteur Dex. Côté cluster, ces variables sont commentées dans `deployment-api.yaml` du dépôt infrastructure pour la démo.

## Déploiement

Le déploiement passe par GitOps depuis le dépôt infrastructure `projet-etude-M1`, qui porte les manifests Kubernetes (`kubernetes/apps/projet-etude-app-demo/`), la CI et ArgoCD. Voir le DAT du projet pour l'architecture complète.
