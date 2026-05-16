package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const stateCookie = "app_oidc_state"

// Login expose les handlers /auth/login, /auth/callback et /auth/logout.
// Le state OIDC est stocké dans un cookie court signé (anti-CSRF léger).
type Login struct {
	cfg      Config
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

func NewLogin(ctx context.Context, cfg Config) (*Login, error) {
	if !cfg.Enabled {
		return &Login{cfg: cfg}, nil
	}
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, err
	}
	return &Login{
		cfg: cfg,
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email", "groups"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
	}, nil
}

func (l *Login) Enabled() bool { return l.cfg.Enabled }

func (l *Login) StartHandler(w http.ResponseWriter, r *http.Request) {
	if !l.cfg.Enabled {
		http.Error(w, "auth disabled\n", http.StatusServiceUnavailable)
		return
	}
	state := randomState()
	// Cookie state OIDC : Secure conditionnel pour rester utilisable
	// derrière port-forward HTTP en lab. En prod, Tailscale-serve = HTTPS.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure géré dynamiquement
		Name:     stateCookie,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.URL.Scheme == "https" || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
		MaxAge:   300,
	})
	http.Redirect(w, r, l.oauth.AuthCodeURL(state), http.StatusFound)
}

func (l *Login) CallbackHandler(w http.ResponseWriter, r *http.Request) {
	if !l.cfg.Enabled {
		http.Error(w, "auth disabled\n", http.StatusServiceUnavailable)
		return
	}
	c, err := r.Cookie(stateCookie)
	if err != nil || c.Value == "" || c.Value != r.URL.Query().Get("state") {
		http.Error(w, "bad state\n", http.StatusBadRequest)
		return
	}
	// Cookie state consommé.
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/", MaxAge: -1}) //nolint:gosec // suppression cookie

	token, err := l.oauth.Exchange(r.Context(), r.URL.Query().Get("code"))
	if err != nil {
		http.Error(w, "exchange: "+err.Error(), http.StatusBadGateway)
		return
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(w, "no id_token", http.StatusBadGateway)
		return
	}
	if _, err := l.verifier.Verify(r.Context(), rawIDToken); err != nil {
		http.Error(w, "verify: "+err.Error(), http.StatusUnauthorized)
		return
	}

	sealed := sealCookie(l.cfg.SessionKey, rawIDToken)
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure géré dynamiquement
		Name:     sessionCookieName,
		Value:    sealed,
		Path:     "/",
		HttpOnly: true,
		Secure:   r.URL.Scheme == "https" || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
		MaxAge:   3600,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (l *Login) LogoutHandler(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Path: "/", MaxAge: -1}) //nolint:gosec // suppression cookie
	http.Redirect(w, r, "/", http.StatusFound)
}

func randomState() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}
