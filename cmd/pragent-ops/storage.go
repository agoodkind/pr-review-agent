package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
)

func storage(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) error {
	var runtimePath, tokenPath string
	set := flag.NewFlagSet("storage", flag.ContinueOnError)
	set.SetOutput(stderr)
	set.StringVar(&runtimePath, "runtime", "runtime.json", "Public provider runtime configuration.")
	set.StringVar(&tokenPath, "operator-token-file", "", "File containing the registered operator token.")
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if set.NArg() != 0 || tokenPath == "" {
		return errors.New("storage requires --operator-token-file and accepts no positional arguments")
	}
	data, err := os.ReadFile(runtimePath)
	if err != nil {
		slog.WarnContext(ctx, "Public runtime configuration read failed", "path", runtimePath)
		return fmt.Errorf("read public runtime configuration: %w", err)
	}
	var configuration counterRuntime
	if json.Unmarshal(data, &configuration) != nil || configuration.URL == "" {
		return errors.New("runtime configuration requires PROVIDER_BUDGET_URL")
	}
	client, err := counterClient(configuration.URL, "", tokenPath)
	if err != nil {
		return err
	}
	result, err := client.Storage(ctx)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return fmt.Errorf("encode usage storage statistics: %w", err)
	}
	return nil
}
