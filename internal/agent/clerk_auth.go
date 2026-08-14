package agent

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const maxClerkTokenBytes = 16 << 10

type bearerAuthenticator interface {
	Authorize(context.Context, string) bool
}

type clerkSessionClaims struct {
	jwt.RegisteredClaims
	AuthorizedParty string `json:"azp"`
	SessionID       string `json:"sid"`
}

type clerkSessionAuthenticator struct {
	issuer          string
	jwksURL         string
	allowedParties  map[string]struct{}
	allowedSubjects map[string]struct{}
	client          *http.Client
	now             func() time.Time

	mu           sync.RWMutex
	keys         map[string]*rsa.PublicKey
	refreshAfter time.Time
}

type clerkJWKSet struct {
	Keys []clerkJWK `json:"keys"`
}

type clerkJWK struct {
	KeyID     string `json:"kid"`
	KeyType   string `json:"kty"`
	Algorithm string `json:"alg"`
	Use       string `json:"use"`
	Modulus   string `json:"n"`
	Exponent  string `json:"e"`
}

func newClerkSessionAuthenticatorFromEnv() (*clerkSessionAuthenticator, error) {
	issuer := strings.TrimRight(strings.TrimSpace(os.Getenv("POOLCTL_CLERK_ISSUER")), "/")
	parties := parseCSVSet(os.Getenv("POOLCTL_CLERK_AUTHORIZED_PARTIES"))
	subjects := parseCSVSet(os.Getenv("POOLCTL_CLERK_OPERATOR_USER_IDS"))
	if issuer == "" && len(parties) == 0 && len(subjects) == 0 {
		return nil, nil
	}
	if issuer == "" || len(parties) == 0 || len(subjects) == 0 {
		return nil, errors.New("POOLCTL_CLERK_ISSUER, POOLCTL_CLERK_AUTHORIZED_PARTIES, and POOLCTL_CLERK_OPERATOR_USER_IDS must be configured together")
	}
	parsed, err := url.Parse(issuer)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("POOLCTL_CLERK_ISSUER must be an HTTPS origin without a path")
	}
	for party := range parties {
		partyURL, err := url.Parse(party)
		if err != nil || partyURL.Scheme != "https" || partyURL.Host == "" || partyURL.Path != "" || partyURL.RawQuery != "" || partyURL.Fragment != "" {
			return nil, fmt.Errorf("invalid Clerk authorized party %q: expected an HTTPS origin", party)
		}
	}
	return newClerkSessionAuthenticator(issuer, issuer+"/.well-known/jwks.json", parties, subjects, &http.Client{
		Timeout: 8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("Clerk JWKS redirects are not allowed")
		},
	}), nil
}

func newClerkSessionAuthenticator(issuer, jwksURL string, parties, subjects map[string]struct{}, client *http.Client) *clerkSessionAuthenticator {
	return &clerkSessionAuthenticator{
		issuer: issuer, jwksURL: jwksURL,
		allowedParties: parties, allowedSubjects: subjects,
		client: client, now: time.Now, keys: map[string]*rsa.PublicKey{},
	}
}

func parseCSVSet(raw string) map[string]struct{} {
	values := map[string]struct{}{}
	for _, value := range strings.Split(raw, ",") {
		if value = strings.TrimSpace(value); value != "" {
			values[value] = struct{}{}
		}
	}
	return values
}

func (a *clerkSessionAuthenticator) Authorize(ctx context.Context, raw string) bool {
	if a == nil || raw == "" || len(raw) > maxClerkTokenBytes {
		return false
	}
	claims := &clerkSessionClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodRS256 {
			return nil, fmt.Errorf("unexpected JWT signing method %q", token.Method.Alg())
		}
		keyID, _ := token.Header["kid"].(string)
		if keyID == "" {
			return nil, errors.New("JWT is missing kid")
		}
		return a.key(ctx, keyID)
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithIssuer(a.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(5*time.Second),
		jwt.WithTimeFunc(a.now),
	)
	if err != nil || !token.Valid || claims.Subject == "" || claims.SessionID == "" || claims.AuthorizedParty == "" || claims.IssuedAt == nil {
		return false
	}
	if _, ok := a.allowedSubjects[claims.Subject]; !ok {
		return false
	}
	_, ok := a.allowedParties[claims.AuthorizedParty]
	return ok
}

func (a *clerkSessionAuthenticator) key(ctx context.Context, keyID string) (*rsa.PublicKey, error) {
	now := a.now()
	a.mu.RLock()
	key := a.keys[keyID]
	fresh := now.Before(a.refreshAfter)
	a.mu.RUnlock()
	if key != nil && fresh {
		return key, nil
	}
	if err := a.refreshKeys(ctx, key == nil); err != nil {
		return nil, err
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	key = a.keys[keyID]
	if key == nil {
		return nil, fmt.Errorf("Clerk JWKS does not contain key %q", keyID)
	}
	return key, nil
}

func (a *clerkSessionAuthenticator) refreshKeys(ctx context.Context, force bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !force && a.now().Before(a.refreshAfter) && len(a.keys) > 0 {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.jwksURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch Clerk JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch Clerk JWKS: HTTP %d", resp.StatusCode)
	}
	var set clerkJWKSet
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	if err := decoder.Decode(&set); err != nil {
		return fmt.Errorf("decode Clerk JWKS: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, candidate := range set.Keys {
		if candidate.KeyID == "" || candidate.KeyType != "RSA" || candidate.Modulus == "" || candidate.Exponent == "" {
			continue
		}
		if candidate.Algorithm != "" && candidate.Algorithm != jwt.SigningMethodRS256.Alg() {
			continue
		}
		if candidate.Use != "" && candidate.Use != "sig" {
			continue
		}
		publicKey, err := rsaPublicKey(candidate.Modulus, candidate.Exponent)
		if err != nil {
			continue
		}
		keys[candidate.KeyID] = publicKey
	}
	if len(keys) == 0 {
		return errors.New("Clerk JWKS contained no usable RSA signing keys")
	}
	a.keys = keys
	a.refreshAfter = a.now().Add(15 * time.Minute)
	return nil
}

func rsaPublicKey(modulus, exponent string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(modulus)
	if err != nil || len(nBytes) == 0 {
		return nil, errors.New("invalid RSA modulus")
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(exponent)
	if err != nil || len(eBytes) == 0 || len(eBytes) > 4 {
		return nil, errors.New("invalid RSA exponent")
	}
	exponentValue := 0
	for _, value := range eBytes {
		exponentValue = exponentValue<<8 | int(value)
	}
	if exponentValue < 3 || exponentValue%2 == 0 {
		return nil, errors.New("invalid RSA exponent")
	}
	publicKey := &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: exponentValue}
	if publicKey.N.BitLen() < 2048 {
		return nil, errors.New("RSA modulus is too small")
	}
	return publicKey, nil
}
