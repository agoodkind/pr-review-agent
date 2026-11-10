package cloudflareops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
)

var (
	errScopedCredentialInvalid    = errors.New("cloudflare returned an invalid scoped credential")
	errScopedCredentialIncomplete = errors.New(
		"cloudflare did not return a complete scoped credential",
	)
)

func (credentials *Credentials) acquireScoped(
	ctx context.Context,
	options CredentialOptions,
) error {
	body, err := credentials.accountClient.tokenBody(ctx, options)
	if err != nil {
		return err
	}

	created, err := credentials.accountClient.createToken(ctx, body)
	if err != nil {
		return err
	}

	var scoped tokenResult
	if json.Unmarshal(created.Result, &scoped) != nil {
		return errScopedCredentialInvalid
	}

	credentials.id = scoped.ID
	if scoped.ID == "" || scoped.Value == "" {
		return errScopedCredentialIncomplete
	}

	credentials.Client, err = NewClient(options.APIURL, scoped.Value, nil)
	if err != nil {
		return err
	}

	if err = os.WriteFile(
		filepath.Join(credentials.directory, "token"),
		[]byte(scoped.Value),
		0o600,
	); err != nil {
		slog.WarnContext(ctx, "Private temporary credential write failed")

		return fmt.Errorf("write private temporary credential: %w", err)
	}

	if err = os.WriteFile(
		filepath.Join(credentials.directory, "id"),
		[]byte(scoped.ID),
		0o600,
	); err != nil {
		slog.WarnContext(ctx, "Temporary credential identifier write failed")

		return fmt.Errorf("write temporary credential identifier: %w", err)
	}

	_, _, err = credentials.Client.request(ctx, http.MethodGet, "/user/tokens/verify", nil)
	if err != nil {
		slog.WarnContext(ctx, "Scoped credential verification failed")

		return fmt.Errorf("scoped credential verification failed: %w", err)
	}

	return nil
}
