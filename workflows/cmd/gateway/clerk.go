package main

// Clerk JWT verification against the project's own public JWKS endpoint —
// no Clerk secret key involved. Deliberately mirrors agent-brain's own
// internal/auth/clerk.go almost line for line rather than using the
// clerk-sdk-go package this file used until now: that SDK's jwt.Verify goes
// through Clerk's authenticated backend API by default (clerk.SetKey(secret)
// + GetBackend()), which is why this Gateway originally needed a
// CLERK_SECRET_KEY that nothing else in this infra has ever provisioned.
// agent-brain's explorer API verifies the exact same Clerk project's tokens
// today using only CLERK_JWKS_URL/CLERK_ISSUER — both plain, non-secret
// values (agent-brain's own chart even commits clerkIssuer as a values.yaml
// default) — via the public /.well-known/jwks.json endpoint every OIDC-style
// provider exposes for exactly this offline-verification use case. Same
// mechanism here removes the secret-provisioning problem entirely instead of
// working around it.
import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var errInvalidToken = errors.New("invalid token")

type clerkConfig struct {
	JWKSURL string
	Issuer  string
}

// clerkConfigFromEnv builds clerkConfig from CLERK_JWKS_URL and optional
// CLERK_ISSUER (CLERK_JWKS_URL, if set, wins — same precedence as
// agent-brain's own auth.ClerkConfigFromEnv).
func clerkConfigFromEnv() clerkConfig {
	jwksURL := strings.TrimSpace(envOrDefault("CLERK_JWKS_URL", ""))
	issuer := strings.TrimSpace(envOrDefault("CLERK_ISSUER", ""))
	if jwksURL == "" && issuer != "" {
		jwksURL = strings.TrimSuffix(issuer, "/") + "/.well-known/jwks.json"
	}
	return clerkConfig{JWKSURL: jwksURL, Issuer: issuer}
}

// verifyClerkSessionJWT validates a Clerk-issued Bearer JWT (RS256) and
// returns the subject (Clerk user_id).
func verifyClerkSessionJWT(ctx context.Context, cfg clerkConfig, tokenStr string) (string, error) {
	if cfg.JWKSURL == "" {
		return "", errors.New("clerk jwks not configured")
	}
	tokenStr = strings.TrimSpace(tokenStr)
	if tokenStr == "" {
		return "", errInvalidToken
	}

	key, err := clerkJWKS.getKey(ctx, cfg.JWKSURL, tokenStr)
	if err != nil {
		return "", err
	}

	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, errInvalidToken
		}
		return key, nil
	}, jwt.WithValidMethods([]string{"RS256"}))
	if err != nil || !token.Valid {
		return "", errInvalidToken
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", errInvalidToken
	}
	if cfg.Issuer != "" {
		iss, _ := claims["iss"].(string)
		if iss != cfg.Issuer {
			return "", errInvalidToken
		}
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return "", errInvalidToken
	}
	return sub, nil
}

type jwksCache struct {
	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
	ttl     time.Duration
}

var clerkJWKS = &jwksCache{ttl: 15 * time.Minute}

func (c *jwksCache) getKey(ctx context.Context, jwksURL, tokenStr string) (*rsa.PublicKey, error) {
	kid, err := jwtKid(tokenStr)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	if c.keys != nil && time.Since(c.fetched) < c.ttl {
		if k, ok := c.keys[kid]; ok {
			c.mu.Unlock()
			return k, nil
		}
	}
	c.mu.Unlock()

	if err := c.refresh(ctx, jwksURL); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k, ok := c.keys[kid]
	if !ok {
		return nil, errInvalidToken
	}
	return k, nil
}

func (c *jwksCache) refresh(ctx context.Context, jwksURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURL, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks fetch: %s", res.Status)
	}
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
			Kty string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(res.Body).Decode(&doc); err != nil {
		return err
	}
	keys := make(map[string]*rsa.PublicKey)
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		pub, err := rsaFromModExp(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return errors.New("jwks: no rsa keys")
	}
	c.mu.Lock()
	c.keys = keys
	c.fetched = time.Now()
	c.mu.Unlock()
	return nil
}

func jwtKid(tokenStr string) (string, error) {
	parts := strings.Split(tokenStr, ".")
	if len(parts) < 2 {
		return "", errInvalidToken
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", errInvalidToken
	}
	var hdr struct {
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(raw, &hdr); err != nil || hdr.Kid == "" {
		return "", errInvalidToken
	}
	return hdr.Kid, nil
}

func rsaFromModExp(nB64, eB64 string) (*rsa.PublicKey, error) {
	nb, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, err
	}
	eb, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, err
	}
	n := new(big.Int).SetBytes(nb)
	e := 0
	for _, b := range eb {
		e = e<<8 + int(b)
	}
	if e == 0 {
		e = 65537
	}
	return &rsa.PublicKey{N: n, E: e}, nil
}
