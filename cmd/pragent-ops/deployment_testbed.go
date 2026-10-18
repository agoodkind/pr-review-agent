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

func deploymentTest(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var options cloudflareops.DeploymentTestOptions
	set := flag.NewFlagSet("deployment-test", flag.ContinueOnError)
	set.SetOutput(stderr)
	set.StringVar(&options.Deployment.Wrangler, "wrangler", "", "Prepared isolated gateway Wrangler configuration.")
	set.StringVar(&options.Deployment.ServiceWrangler, "service-wrangler", "", "Prepared isolated service Wrangler configuration.")
	set.StringVar(&options.Deployment.Configuration, "deployment", "", "Prepared isolated public deployment configuration.")
	set.StringVar(&options.Deployment.Image, "image", "", "Actual isolated candidate image digest.")
	set.StringVar(&options.Deployment.Revision, "revision", "", "Actual full candidate commit.")
	set.StringVar(&options.BaselineImage, "baseline-image", "", "Actual isolated baseline image digest.")
	set.StringVar(&options.BaselineRevision, "baseline-revision", "", "Actual full baseline commit.")
	set.StringVar(&options.BaselineWrangler, "baseline-wrangler", "", "Prepared actual prior Worker configuration for isolated bootstrap.")
	set.StringVar(&options.Deployment.TokenFile, "token-file", "", "Existing deployment credential file.")
	set.StringVar(&options.Deployment.AccountTokenFile, "account-token-file", "", "Account token file for temporary scoped deployment permission.")
	set.DurationVar(&options.Deployment.CredentialTTL, "credential-ttl", 90*time.Minute, "Temporary deployment credential expiration.")
	set.StringVar(&options.Deployment.Directory, "output-dir", "", "New private evidence directory.")
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if set.NArg() != 0 || (options.Deployment.TokenFile == "") == (options.Deployment.AccountTokenFile == "") {
		return errors.New("deployment-test requires exactly one token file and accepts no positional arguments")
	}
	result, err := cloudflareops.TestDeployment(ctx, options)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(result)
}
