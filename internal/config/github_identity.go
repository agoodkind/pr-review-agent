package config

import (
	"errors"
	"net/url"
	"strings"
)

// LoadGitHubIdentity does not require provider or webhook credentials.
func LoadGitHubIdentity(appID int64, botLogin string, privateKey []byte) (Config, error) {
	var cfg Config
	if appID <= 0 {
		return cfg, errors.New("GitHub app ID must be positive")
	}
	if botLogin == "" || strings.TrimSpace(botLogin) != botLogin {
		return cfg, errors.New("GitHub bot login must be configured without surrounding whitespace")
	}
	key, err := parseRSAPrivateKey(string(privateKey))
	if err != nil {
		return cfg, errors.New("GitHub app private key must contain a valid RSA private key")
	}
	cfg.GitHubPrivateKey = key // gitleaks:allow
	cfg.GitHubAppID = appID
	cfg.GitHubBotLogin = botLogin
	cfg.GitHubAPIBaseURL, err = url.Parse("https://api.github.com")
	if err != nil {
		return cfg, errors.New("GitHub REST API URL could not be configured")
	}
	cfg.GitHubGraphQLURL, err = url.Parse("https://api.github.com/graphql")
	if err != nil {
		return cfg, errors.New("GitHub GraphQL API URL could not be configured")
	}
	return cfg, nil
}
