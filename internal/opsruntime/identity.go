package opsruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"goodkind.io/pr-review-agent/internal/cloudflareops"
	"goodkind.io/pr-review-agent/internal/config"
)

var (
	errUnreadableRuntime     = errors.New("public runtime configuration could not be read")
	errInvalidIdentity       = errors.New("public runtime GitHub identity could not be decoded")
	errInvalidAppID          = errors.New("GitHub app ID must be a positive integer")
	errUnreadableSigningFile = errors.New("GitHub app private-key file could not be read")
)

// ReviewBodyIdentity loads the app identity for review body commands.
//
// For example:
//
//	cfg, err := ReviewBodyIdentity(path, appID, bot, keyPath)
func ReviewBodyIdentity(
	runtimePath string,
	appIDText string,
	botLogin string,
	privateKeyFile string,
) (config.Config, error) {
	var cfg config.Config

	if appIDText == "" || botLogin == "" {
		data, err := os.ReadFile(runtimePath)
		if err != nil {
			return cfg, errUnreadableRuntime
		}

		var runtime ReviewBodyRuntime
		if err := json.Unmarshal(data, &runtime); err != nil {
			return cfg, errInvalidIdentity
		}

		if appIDText == "" {
			appIDText = runtime.AppID
		}

		if botLogin == "" {
			botLogin = runtime.BotLogin
		}
	}

	appID, err := strconv.ParseInt(appIDText, 10, 64)
	if err != nil || appID <= 0 {
		return cfg, errInvalidAppID
	}

	key, err := cloudflareops.ReadCredential(privateKeyFile)
	if err != nil {
		return cfg, errUnreadableSigningFile
	}

	identity, err := config.LoadGitHubIdentity(appID, botLogin, key)
	if err != nil {
		slog.Warn("Diagnostic GitHub identity loading failed", "err", err)

		return cfg, fmt.Errorf("load GitHub identity: %w", err)
	}

	return identity, nil
}
