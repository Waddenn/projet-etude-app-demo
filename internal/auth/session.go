package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

const sessionCookieName = "app_session"

// sealCookie HMAC-SHA256 du token avec la clé partagée. Format : <token>.<sig>.
// Évite une dépendance externe (gorilla/sessions, etc.) — l'objectif est de
// stocker un ID token signé par Dex, dont on relit le sub côté serveur.
func sealCookie(key []byte, token string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(token))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(token)) + "." + sig
}

func unsealCookie(key []byte, value string) (string, bool) {
	if len(key) == 0 {
		return "", false
	}
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	tokB, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	expected := hmac.New(sha256.New, key)
	expected.Write(tokB)
	want := base64.RawURLEncoding.EncodeToString(expected.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[1])) {
		return "", false
	}
	return string(tokB), true
}
