package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// GoogleJWKSURL is where Google publishes the RSA keys its ID tokens are
// signed with.
const GoogleJWKSURL = "https://www.googleapis.com/oauth2/v3/certs"

const (
	// jwksTTL is how long a fetched key set is trusted before refetching.
	// Google rotates keys on the order of days; an hour keeps the window for
	// serving a stale set small without hammering the endpoint.
	jwksTTL = time.Hour
	// jwksRefetchCooldown bounds how often an unknown kid can force a refetch.
	// Without it, a stream of forged tokens with made-up kids would turn every
	// failed login into an outbound request to Google.
	jwksRefetchCooldown = time.Minute
)

// JWKSCache fetches and caches an RSA JWK set.
//
// It refetches when the TTL lapses and, subject to a cooldown, when asked for
// a kid it does not hold — that is what key rotation looks like from here.
type JWKSCache struct {
	url  string
	http *http.Client
	ttl  time.Duration

	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	fetched time.Time
}

// NewJWKSCache builds a cache over the JWK set at url. httpClient nil means
// a default client with a timeout; tests pass an httptest server's client.
func NewJWKSCache(url string, httpClient *http.Client) *JWKSCache {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &JWKSCache{url: url, http: httpClient, ttl: jwksTTL}
}

// Key returns the RSA public key with the given kid, fetching or refetching
// the set as needed.
func (c *JWKSCache) Key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	stale := time.Since(c.fetched) >= c.ttl
	if key, ok := c.keys[kid]; ok && !stale {
		return key, nil
	}
	// Unknown kid or stale set: refetch, but never more often than the
	// cooldown — a fresh fetch that still lacks the kid means the token is
	// forged or ancient, not that Google rotated twice in a minute.
	if stale || time.Since(c.fetched) >= jwksRefetchCooldown {
		if err := c.fetchLocked(ctx); err != nil {
			return nil, err
		}
	}
	key, ok := c.keys[kid]
	if !ok {
		return nil, fmt.Errorf("auth: no JWKS key with kid %q", kid)
	}
	return key, nil
}

// fetchLocked replaces the cached key set. The caller holds c.mu.
func (c *JWKSCache) fetchLocked(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("auth: build JWKS request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("auth: fetch JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("auth: JWKS endpoint returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("auth: read JWKS response: %w", err)
	}

	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			Use string `json:"use"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return fmt.Errorf("auth: parse JWKS: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{
			N: new(big.Int).SetBytes(n),
			E: int(new(big.Int).SetBytes(e).Int64()),
		}
	}
	if len(keys) == 0 {
		return fmt.Errorf("auth: JWKS at %s held no usable RSA keys", c.url)
	}
	c.keys = keys
	c.fetched = time.Now()
	return nil
}
