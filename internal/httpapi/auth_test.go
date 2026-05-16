package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/Waddenn/projet-etude-app-demo/internal/auth"
	"github.com/Waddenn/projet-etude-app-demo/internal/store"
	"github.com/Waddenn/projet-etude-app-demo/internal/testutil"
)

type fakeIDP struct {
	server *httptest.Server
	key    *rsa.PrivateKey
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIDP{key: key}
	mux := http.NewServeMux()
	idp.server = httptest.NewServer(mux)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.server.URL,
			"jwks_uri":                              idp.server.URL + "/keys",
			"authorization_endpoint":                idp.server.URL + "/auth",
			"token_endpoint":                        idp.server.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{
			Keys: []jose.JSONWebKey{{
				Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig",
			}},
		})
	})
	t.Cleanup(idp.server.Close)
	return idp
}

func (i *fakeIDP) tokenFor(t *testing.T, sub, email string, groups []string) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: i.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(jwt.Claims{
		Issuer:   i.server.URL,
		Audience: jwt.Audience{"test-client"},
		Subject:  sub,
		Expiry:   jwt.NewNumericDate(time.Now().Add(10 * time.Minute)),
		IssuedAt: jwt.NewNumericDate(time.Now()),
	}).Claims(map[string]any{"email": email, "groups": groups}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func setupWithAuth(t *testing.T) (http.Handler, *store.Store, *fakeIDP) {
	t.Helper()
	pool := testutil.NewPostgres(t)
	st := store.New(pool)
	idp := newFakeIDP(t)
	a, err := auth.New(context.Background(), auth.Config{
		IssuerURL: idp.server.URL, ClientID: "test-client",
		ViewerGroup: "viewers", OperatorGroup: "operators", Enabled: true,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return NewMux(st, a, nil), st, idp
}

func TestE2E_Anonymous_Denied(t *testing.T) {
	mux, _, _ := setupWithAuth(t)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tickets", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/tickets unauth: got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/tickets",
		strings.NewReader(`{"title":"x","priority":"low"}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST /api/tickets unauth: got %d", rec.Code)
	}
}

func TestE2E_Viewer_CannotMutate(t *testing.T) {
	mux, _, idp := setupWithAuth(t)
	tok := idp.tokenFor(t, "bob", "bob@example.com", []string{"viewers"})

	// Lecture autorisée
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tickets", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer GET should be 200, got %d", rec.Code)
	}

	// Mutation interdite
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/tickets",
		strings.NewReader(`{"title":"x","priority":"low"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("viewer POST should be 403, got %d (body=%s)", rec.Code, rec.Body)
	}
}

func TestE2E_Operator_CanMutate_AndAuditLogged(t *testing.T) {
	mux, st, idp := setupWithAuth(t)
	tok := idp.tokenFor(t, "alice", "alice@example.com",
		[]string{"viewers", "operators"})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tickets",
		strings.NewReader(`{"title":"prod down","priority":"high"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("operator POST should be 201, got %d (body=%s)", rec.Code, rec.Body)
	}

	// Vérifie l'écriture audit avec le bon sub.
	entries, err := st.ListAudit(context.Background(), 10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no audit entry recorded")
	}
	e := entries[0]
	if e.Action != "ticket.create" || e.UserSub != "alice" || e.Result != "ok" {
		t.Fatalf("unexpected audit entry: %+v", e)
	}
}

func TestE2E_PublicEndpoints_RemainOpen(t *testing.T) {
	mux, _, _ := setupWithAuth(t)
	for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s should be 200 even without auth, got %d", p, rec.Code)
		}
	}
}
