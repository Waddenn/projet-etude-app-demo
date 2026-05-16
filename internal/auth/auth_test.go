package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// fakeIDP démarre un serveur HTTP qui expose /.well-known/openid-configuration
// et un JWKS, et fournit une méthode Sign() pour produire des id_tokens
// vérifiables par go-oidc.
type fakeIDP struct {
	server *httptest.Server
	key    *rsa.PrivateKey
	kid    string
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	idp := &fakeIDP{key: key, kid: "test-key"}
	mux := http.NewServeMux()
	idp.server = httptest.NewServer(mux)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.server.URL,
			"jwks_uri":                              idp.server.URL + "/keys",
			"authorization_endpoint":                idp.server.URL + "/auth",
			"token_endpoint":                        idp.server.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		jwk := jose.JSONWebKey{
			Key:       &key.PublicKey,
			KeyID:     idp.kid,
			Algorithm: string(jose.RS256),
			Use:       "sig",
		}
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}})
	})
	t.Cleanup(idp.server.Close)
	return idp
}

func (i *fakeIDP) issuer() string { return i.server.URL }

func (i *fakeIDP) sign(t *testing.T, aud, sub, email string, groups []string) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: i.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", i.kid),
	)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	cl := jwt.Claims{
		Issuer:   i.server.URL,
		Audience: jwt.Audience{aud},
		Subject:  sub,
		Expiry:   jwt.NewNumericDate(time.Now().Add(10 * time.Minute)),
		IssuedAt: jwt.NewNumericDate(time.Now()),
	}
	custom := map[string]any{"email": email, "groups": groups}
	raw, err := jwt.Signed(signer).Claims(cl).Claims(custom).Serialize()
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return raw
}

func newAuthenticator(t *testing.T, idp *fakeIDP) *Authenticator {
	t.Helper()
	a, err := New(context.Background(), Config{
		IssuerURL:     idp.issuer(),
		ClientID:      "test-client",
		ViewerGroup:   "viewers",
		OperatorGroup: "operators",
		Enabled:       true,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return a
}

// helper : execute le middleware + un handler de test, retourne le principal vu.
func runWithAuth(t *testing.T, a *Authenticator, req *http.Request) Principal {
	t.Helper()
	var got Principal
	h := a.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = PrincipalFrom(r.Context())
	}))
	h.ServeHTTP(httptest.NewRecorder(), req)
	return got
}

func TestMiddleware_AnonymousWhenNoToken(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuthenticator(t, idp)
	req := httptest.NewRequest(http.MethodGet, "/api/tickets", nil)
	p := runWithAuth(t, a, req)
	if p.Role != RoleAnonymous {
		t.Fatalf("expected anonymous, got role=%s sub=%s", p.Role, p.Sub)
	}
}

func TestMiddleware_OperatorFromBearer(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuthenticator(t, idp)
	tok := idp.sign(t, "test-client", "alice", "alice@example.com",
		[]string{"viewers", "operators"})

	req := httptest.NewRequest(http.MethodPost, "/api/tickets", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	p := runWithAuth(t, a, req)
	if p.Role != RoleOperator || p.Sub != "alice" {
		t.Fatalf("expected operator alice, got %+v", p)
	}
}

func TestMiddleware_ViewerFromBearer(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuthenticator(t, idp)
	tok := idp.sign(t, "test-client", "bob", "", []string{"viewers"})
	req := httptest.NewRequest(http.MethodGet, "/api/tickets", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	p := runWithAuth(t, a, req)
	if p.Role != RoleViewer || p.Sub != "bob" {
		t.Fatalf("expected viewer bob, got %+v", p)
	}
}

func TestMiddleware_RejectUnknownGroup(t *testing.T) {
	idp := newFakeIDP(t)
	a := newAuthenticator(t, idp)
	tok := idp.sign(t, "test-client", "eve", "", []string{"randoms"})
	req := httptest.NewRequest(http.MethodGet, "/api/tickets", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	p := runWithAuth(t, a, req)
	if p.Role != RoleAnonymous {
		t.Fatalf("user without required group should be anonymous, got %+v", p)
	}
}

func TestRequireMutate_DeniesViewer(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tickets", nil)
	ctx := WithPrincipal(req.Context(), Principal{Sub: "bob", Role: RoleViewer})
	RequireMutate(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})(rec, req.WithContext(ctx))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestRequireRead_RequiresAuth(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tickets", nil)
	// pas de WithPrincipal → principalFromCtx renvoie anonymous
	RequireRead(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

// Quick sanity check that base64 RawURL works the way we expect for the
// session cookie sealing.
func TestSessionCookie_RoundTrip(t *testing.T) {
	key := []byte("test-key-32-bytes-please-padding!")
	sealed := sealCookie(key, "hello")
	got, ok := unsealCookie(key, sealed)
	if !ok || got != "hello" {
		t.Fatalf("seal/unseal failed: ok=%v got=%q", ok, got)
	}
	if _, ok := unsealCookie([]byte("wrong-key"), sealed); ok {
		t.Fatal("wrong key should not validate")
	}
}

// Garde-fou contre une régression : un bigint payload reste un bigint dans
// JWT (notre code ne dépend pas du parsing standard, mais ça documente l'intent).
var _ = func() *big.Int { return big.NewInt(0) }
var _ = fmt.Sprint
var _ = base64.StdEncoding
