package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestClerkSessionAuthenticatorValidatesIdentityOriginAndLifetime(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const keyID = "test-key"
	var requests atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		requests.Add(1)
		body, err := json.Marshal(clerkJWKSet{Keys: []clerkJWK{{
			KeyID: keyID, KeyType: "RSA", Algorithm: "RS256", Use: "sig",
			Modulus:  base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()),
			Exponent: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(privateKey.PublicKey.E)).Bytes()),
		}}})
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}

	now := time.Unix(1_800_000_000, 0)
	auth := newClerkSessionAuthenticator(
		"https://clerk.example.test", "https://clerk.example.test/.well-known/jwks.json",
		map[string]struct{}{"https://control.example.test": {}},
		map[string]struct{}{"user_operator": {}},
		client,
	)
	auth.now = func() time.Time { return now }

	valid := clerkTestToken(t, privateKey, keyID, clerkSessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "https://clerk.example.test", Subject: "user_operator",
			IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
		},
		AuthorizedParty: "https://control.example.test", SessionID: "sess_test",
	})
	if !auth.Authorize(context.Background(), valid) {
		t.Fatal("valid Clerk session was rejected")
	}
	if !auth.Authorize(context.Background(), valid) || requests.Load() != 1 {
		t.Fatalf("cached verification failed; JWKS requests = %d", requests.Load())
	}

	for name, mutate := range map[string]func(*clerkSessionClaims){
		"wrong subject": func(claims *clerkSessionClaims) { claims.Subject = "user_other" },
		"wrong party":   func(claims *clerkSessionClaims) { claims.AuthorizedParty = "https://evil.example" },
		"expired":       func(claims *clerkSessionClaims) { claims.ExpiresAt = jwt.NewNumericDate(now.Add(-time.Minute)) },
		"missing issued at": func(claims *clerkSessionClaims) {
			claims.IssuedAt = nil
		},
		"missing session": func(claims *clerkSessionClaims) {
			claims.SessionID = ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			claims := clerkSessionClaims{
				RegisteredClaims: jwt.RegisteredClaims{
					Issuer: "https://clerk.example.test", Subject: "user_operator",
					IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
				},
				AuthorizedParty: "https://control.example.test", SessionID: "sess_test",
			}
			mutate(&claims)
			if auth.Authorize(context.Background(), clerkTestToken(t, privateKey, keyID, claims)) {
				t.Fatal("invalid Clerk session was authorized")
			}
		})
	}
}

func TestClerkSessionAuthenticatorRejectsUnknownKeyAndAlgorithm(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		body, err := json.Marshal(clerkJWKSet{Keys: []clerkJWK{{
			KeyID: "known", KeyType: "RSA", Algorithm: "RS256", Use: "sig",
			Modulus:  base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()),
			Exponent: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(privateKey.PublicKey.E)).Bytes()),
		}}})
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}

	now := time.Unix(1_800_000_000, 0)
	auth := newClerkSessionAuthenticator("https://clerk.example.test", "https://clerk.example.test/.well-known/jwks.json",
		map[string]struct{}{"https://control.example.test": {}}, map[string]struct{}{"user_operator": {}}, client)
	auth.now = func() time.Time { return now }
	claims := clerkSessionClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "https://clerk.example.test", Subject: "user_operator",
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
		},
		AuthorizedParty: "https://control.example.test", SessionID: "sess_test",
	}
	if auth.Authorize(context.Background(), clerkTestToken(t, privateKey, "unknown", claims)) {
		t.Fatal("unknown signing key was authorized")
	}
	hmac := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	hmac.Header["kid"] = "known"
	raw, err := hmac.SignedString([]byte("not-an-rsa-key"))
	if err != nil {
		t.Fatal(err)
	}
	if auth.Authorize(context.Background(), raw) {
		t.Fatal("unexpected signing algorithm was authorized")
	}
}

func clerkTestToken(t *testing.T, key *rsa.PrivateKey, keyID string, claims clerkSessionClaims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = keyID
	raw, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type allowTestBearer struct{ allow bool }

func (a allowTestBearer) Authorize(context.Context, string) bool { return a.allow }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestOperatorAuthorizationKeepsLegacyTokenAndAcceptsClerkSession(t *testing.T) {
	for name, server := range map[string]*server{
		"legacy": {token: "operator-token", operatorAuth: allowTestBearer{allow: false}},
		"clerk":  {token: "operator-token", operatorAuth: allowTestBearer{allow: true}},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/__poolctl/api/status", nil)
			if name == "legacy" {
				req.Header.Set("Authorization", "Bearer operator-token")
			} else {
				req.Header.Set("Authorization", "Bearer clerk-session")
			}
			recorder := httptest.NewRecorder()
			if !server.authorized(recorder, req) {
				t.Fatalf("authorization rejected: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
