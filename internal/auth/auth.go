// Package auth fournit l'authentification OIDC + RBAC applicatif.
//
// Modes :
//   - Si OIDC_ISSUER_URL est vide → mode "open" : toutes les requêtes passent
//     en tant qu'utilisateur anonymous (utile pour les tests, le dev local et
//     les démos sans IdP).
//   - Sinon → JWT Bearer obligatoire sur les routes mutantes ; un cookie de
//     session signé est accepté en alternative pour l'UI HTMX.
//
// RBAC : 2 rôles portés par le claim `groups` (configurable via
// OIDC_VIEWER_GROUP / OIDC_OPERATOR_GROUP).
package auth

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

type Role string

const (
	RoleAnonymous Role = "anonymous"
	RoleViewer    Role = "viewer"
	RoleOperator  Role = "operator"
)

type Principal struct {
	Sub   string
	Email string
	Role  Role
}

func (p Principal) Authenticated() bool { return p.Role != RoleAnonymous }

func (p Principal) CanRead() bool {
	return p.Role == RoleViewer || p.Role == RoleOperator
}

func (p Principal) CanMutate() bool { return p.Role == RoleOperator }

type ctxKey struct{}

func principalFromCtx(ctx context.Context) Principal {
	if v, ok := ctx.Value(ctxKey{}).(Principal); ok {
		return v
	}
	return Principal{Sub: "anonymous", Role: RoleAnonymous}
}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom est l'accesseur public utilisé par les handlers / l'audit.
func PrincipalFrom(ctx context.Context) Principal { return principalFromCtx(ctx) }

// Config est résolu depuis l'environnement.
type Config struct {
	IssuerURL     string
	ClientID      string
	ClientSecret  string
	RedirectURL   string
	ViewerGroup   string
	OperatorGroup string
	// Fallback : si l'IdP n'émet pas de claim `groups` (cas Dex
	// staticPasswords), on accorde RoleOperator aux emails listés ici et
	// RoleViewer aux autres utilisateurs authentifiés.
	OperatorEmails []string
	SessionKey     []byte
	Enabled        bool
}

func ConfigFromEnv() Config {
	c := Config{
		IssuerURL:     os.Getenv("OIDC_ISSUER_URL"),
		ClientID:      os.Getenv("OIDC_CLIENT_ID"),
		ClientSecret:  os.Getenv("OIDC_CLIENT_SECRET"),
		RedirectURL:   os.Getenv("OIDC_REDIRECT_URL"),
		ViewerGroup:   os.Getenv("OIDC_VIEWER_GROUP"),
		OperatorGroup: os.Getenv("OIDC_OPERATOR_GROUP"),
		SessionKey:    []byte(os.Getenv("SESSION_KEY")),
	}
	if c.ViewerGroup == "" {
		c.ViewerGroup = "viewers"
	}
	if c.OperatorGroup == "" {
		c.OperatorGroup = "operators"
	}
	if v := os.Getenv("OIDC_OPERATOR_EMAILS"); v != "" {
		for _, e := range strings.Split(v, ",") {
			if e = strings.TrimSpace(e); e != "" {
				c.OperatorEmails = append(c.OperatorEmails, e)
			}
		}
	}
	c.Enabled = c.IssuerURL != "" && c.ClientID != ""
	return c
}

// Authenticator porte le verifier OIDC + la mapping groups → roles.
type Authenticator struct {
	cfg      Config
	verifier *oidc.IDTokenVerifier
}

func New(ctx context.Context, cfg Config) (*Authenticator, error) {
	if !cfg.Enabled {
		return &Authenticator{cfg: cfg}, nil
	}
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, err
	}
	return &Authenticator{
		cfg:      cfg,
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
	}, nil
}

func (a *Authenticator) Enabled() bool { return a.cfg.Enabled }

// Middleware enrichit le contexte avec le principal résolu. Si l'auth est
// désactivée, on injecte un principal RoleOperator (mode dev local).
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.cfg.Enabled {
			p := Principal{Sub: "dev", Role: RoleOperator}
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
			return
		}
		p, err := a.principalFromRequest(r)
		if err != nil {
			// Pas de credentials valides : on laisse passer en anonymous,
			// l'enforcement se fait dans Require*().
			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(),
				Principal{Sub: "anonymous", Role: RoleAnonymous})))
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

func (a *Authenticator) principalFromRequest(r *http.Request) (Principal, error) {
	// 1. Authorization: Bearer <jwt>
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return a.verifyAndMap(r.Context(), strings.TrimPrefix(h, "Bearer "))
	}
	// 2. Cookie de session (UI)
	if c, err := r.Cookie(sessionCookieName); err == nil {
		if tok, ok := unsealCookie(a.cfg.SessionKey, c.Value); ok {
			return a.verifyAndMap(r.Context(), tok)
		}
	}
	return Principal{}, errors.New("no credentials")
}

func (a *Authenticator) verifyAndMap(ctx context.Context, raw string) (Principal, error) {
	if a.verifier == nil {
		return Principal{}, errors.New("auth disabled")
	}
	tok, err := a.verifier.Verify(ctx, raw)
	if err != nil {
		return Principal{}, err
	}
	var claims struct {
		Email  string   `json:"email"`
		Groups []string `json:"groups"`
	}
	if err := tok.Claims(&claims); err != nil {
		return Principal{}, err
	}
	// Mapping role : groups en priorité, email-list en fallback.
	role := RoleAnonymous
	switch {
	case contains(claims.Groups, a.cfg.OperatorGroup):
		role = RoleOperator
	case contains(claims.Groups, a.cfg.ViewerGroup):
		role = RoleViewer
	case len(claims.Groups) == 0:
		// IdP sans claim groups : fallback email-list.
		if contains(a.cfg.OperatorEmails, claims.Email) {
			role = RoleOperator
		} else {
			role = RoleViewer
		}
	}
	if role == RoleAnonymous {
		return Principal{}, errors.New("user has no authorized group")
	}
	return Principal{Sub: tok.Subject, Email: claims.Email, Role: role}, nil
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// RequireRead exige au minimum RoleViewer. 401 si anonymous, 403 sinon.
func RequireRead(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := PrincipalFrom(r.Context())
		if !p.Authenticated() {
			http.Error(w, "unauthorized\n", http.StatusUnauthorized)
			return
		}
		if !p.CanRead() {
			http.Error(w, "forbidden\n", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// RequireMutate exige RoleOperator.
func RequireMutate(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := PrincipalFrom(r.Context())
		if !p.Authenticated() {
			http.Error(w, "unauthorized\n", http.StatusUnauthorized)
			return
		}
		if !p.CanMutate() {
			http.Error(w, "forbidden\n", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}
