package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"goodkind.io/pr-review-agent/internal/cloudflareops"
)

func registerOperator(ctx context.Context, args []string, stdout, stderr io.Writer) (returnErr error) {
	var opts options
	var operatorTokenFile string
	set := flag.NewFlagSet("operator-token", flag.ContinueOnError)
	set.SetOutput(stderr)
	set.StringVar(&opts.accountTokenFile, "account-token-file", "", "Cloudflare account token file for temporary deployment permission.")
	set.StringVar(&opts.tokenFile, "token-file", "", "Existing Cloudflare token file with Workers Scripts Write permission.")
	set.StringVar(&operatorTokenFile, "operator-token-file", "", "Operator token file; defaults to the supplied Cloudflare credential file.")
	set.StringVar(&opts.wrangler, "wrangler", "deploy/cloudflare/wrangler.jsonc", "Public Wrangler configuration.")
	set.StringVar(&opts.account, "account", "", "Cloudflare account ID; defaults to the Wrangler configuration.")
	set.StringVar(&opts.script, "script", "", "Worker script; defaults to the Wrangler configuration.")
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if set.NArg() != 0 || (opts.tokenFile == "") == (opts.accountTokenFile == "") {
		return errors.New("operator-token requires exactly one --token-file or --account-token-file")
	}
	if err := configuration(&opts); err != nil {
		return err
	}
	if operatorTokenFile == "" {
		operatorTokenFile = opts.tokenFile
		if operatorTokenFile == "" {
			operatorTokenFile = opts.accountTokenFile
		}
	}
	token, err := cloudflareops.ReadCredential(operatorTokenFile)
	if err != nil {
		return err
	}
	credentials, err := cloudflareops.AcquireCredentials(ctx, cloudflareops.CredentialOptions{
		TokenFile: opts.tokenFile, AccountTokenFile: opts.accountTokenFile,
		Account: opts.account, TTL: 15 * time.Minute, PermissionName: "Workers Scripts Write",
	})
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, credentials.Close(ctx)) }()
	if err := credentials.Client.RegisterOperatorToken(ctx, opts.account, opts.script, token); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Worker %s registered the operator token digest. The raw token was not uploaded.\n", opts.script)
	return err
}
