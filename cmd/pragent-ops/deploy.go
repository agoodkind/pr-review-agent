package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"time"

	"goodkind.io/pr-review-agent/internal/cloudflareops"
)

func deploy(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var options cloudflareops.DeploymentOptions
	set := flag.NewFlagSet("deploy", flag.ContinueOnError)
	set.SetOutput(stderr)
	set.StringVar(&options.Wrangler, "wrangler", "deploy/cloudflare/wrangler.jsonc", "Gateway Wrangler template.")
	set.StringVar(&options.ServiceWrangler, "service-wrangler", "deploy/cloudflare/service.wrangler.jsonc", "Independent candidate Wrangler template.")
	set.StringVar(&options.Configuration, "deployment", "deploy/cloudflare/deployment.json", "Public deployment configuration.")
	set.StringVar(&options.Image, "image", "", "Immutable GHCR image digest.")
	set.StringVar(&options.Revision, "revision", "", "Expected full Git revision from the candidate application.")
	set.StringVar(&options.TokenFile, "token-file", "", "Cloudflare token file with Worker and Container deployment permission.")
	set.StringVar(&options.AccountTokenFile, "account-token-file", "", "Account token file used to create and revoke a temporary deployment credential.")
	set.DurationVar(&options.CredentialTTL, "credential-ttl", 90*time.Minute, "Temporary deployment credential expiration; maximum 24h.")
	set.StringVar(&options.Directory, "output-dir", "", "New private deployment evidence directory.")
	set.BoolVar(&options.Preflight, "preflight", false, "Read deployed selections and verify active health without changing Cloudflare.")
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if set.NArg() != 0 || (options.TokenFile == "") == (options.AccountTokenFile == "") {
		return errors.New("deploy requires exactly one --token-file or --account-token-file and accepts no positional arguments")
	}
	result, err := cloudflareops.Deploy(ctx, options)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(result)
}
