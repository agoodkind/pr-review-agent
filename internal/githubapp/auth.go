package githubapp

import (
	"context"
	"crypto"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	tokenRefreshLead = 5 * time.Minute
	jwtLifetime      = 9 * time.Minute
	jwtIssuedSkew    = 60 * time.Second
)

type installationToken struct {
	value     string
	expiresAt time.Time
}

type tokenCache struct {
	mu     sync.Mutex
	now    func() time.Time
	tokens map[int64]installationToken
}

func newTokenCache(now func() time.Time) *tokenCache {
	return &tokenCache{
		mu:     sync.Mutex{},
		now:    now,
		tokens: make(map[int64]installationToken),
	}
}

func (cache *tokenCache) get(installationID int64) (string, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	entry, ok := cache.tokens[installationID]
	if !ok {
		return "", false
	}
	if cache.now().Add(tokenRefreshLead).After(entry.expiresAt) {
		delete(cache.tokens, installationID)
		return "", false
	}
	return entry.value, true
}

func (cache *tokenCache) set(installationID int64, value string, expiresAt time.Time) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.tokens[installationID] = installationToken{
		value:     value,
		expiresAt: expiresAt,
	}
}

type jwtClaims struct {
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	Issuer    string `json:"iss"`
}

func buildAppJWT(privateKey *rsa.PrivateKey, appID int64, now time.Time) (string, error) {
	header := map[string]string{
		"alg": "RS256",
		"typ": "JWT",
	}
	claims := jwtClaims{
		IssuedAt:  now.Add(-jwtIssuedSkew).Unix(),
		ExpiresAt: now.Add(jwtLifetime).Unix(),
		Issuer:    strconv.FormatInt(appID, 10),
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", errors.New("marshal jwt header")
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", errors.New("marshal jwt claims")
	}

	unsigned := encodeJWTPart(headerJSON) + "." + encodeJWTPart(claimsJSON)
	hashed := crypto.SHA256.New()
	_, _ = hashed.Write([]byte(unsigned))
	digest := hashed.Sum(nil)

	signature, err := rsa.SignPKCS1v15(nil, privateKey, crypto.SHA256, digest)
	if err != nil {
		return "", errors.New("sign jwt")
	}

	return unsigned + "." + encodeJWTPart(signature), nil
}

func encodeJWTPart(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}

type installationTokenResponse struct {
	Token               string    `json:"token"`
	ExpiresAt           time.Time `json:"expires_at"`
	RepositorySelection string    `json:"repository_selection"`
	Permissions         struct {
		Contents     string `json:"contents"`
		PullRequests string `json:"pull_requests"`
	} `json:"permissions"`
}

func (client *Client) installationToken(ctx context.Context, installationID int64) (string, error) {
	if token, ok := client.tokens.get(installationID); ok {
		return token, nil
	}

	jwt, err := buildAppJWT(client.cfg.GitHubPrivateKey, client.cfg.GitHubAppID, client.now())
	if err != nil {
		client.logger.ErrorContext(ctx, "build app jwt", slog.String("err", err.Error()))
		return "", errors.New("build app jwt")
	}

	path := fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.restURL(path), http.NoBody)
	if err != nil {
		client.logger.ErrorContext(ctx, "create installation token request", slog.String("err", err.Error()))
		return "", errors.New("create installation token request")
	}
	request.Header.Set("Authorization", "Bearer "+jwt)
	request.Header.Set("Accept", githubAcceptHeader)
	request.Header.Set(githubAPIVersionHeader, githubAPIVersionValue)

	response, err := client.httpClient.Do(request)
	if err != nil {
		client.logger.ErrorContext(ctx, "request installation token", slog.String("err", err.Error()))
		return "", &requestError{stage: "reach GitHub", method: http.MethodPost, path: path, cause: err}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()

	body, err := readLimitedBody(response.Body)
	if err != nil {
		client.logger.ErrorContext(ctx, "read installation token response", slog.String("err", err.Error()))
		return "", &requestError{stage: "read the response", method: http.MethodPost, path: path, cause: err}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return "", newAPIError(http.MethodPost, path, response.StatusCode, sanitizeErrorBody(body))
	}

	var parsed installationTokenResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		client.logger.ErrorContext(ctx, "decode installation token response", slog.String("err", err.Error()))
		return "", errors.New("decode installation token response")
	}
	if strings.TrimSpace(parsed.Token) == "" {
		return "", errors.New("installation token missing")
	}
	client.logger.InfoContext(
		ctx,
		"installation token granted",
		slog.Int64("installation_id", installationID),
		slog.String("repository_selection", parsed.RepositorySelection),
		slog.String("contents_permission", parsed.Permissions.Contents),
		slog.String("pull_requests_permission", parsed.Permissions.PullRequests),
	)

	client.tokens.set(installationID, parsed.Token, parsed.ExpiresAt)
	return parsed.Token, nil
}
