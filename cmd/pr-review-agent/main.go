// Command pr-review-agent runs end-to-end pull request reviews.
package main

import (
	"fmt"
	"log/slog"
	"os"

	"goodkind.io/pr-review-agent/internal/version"
)

func main() {
	slog.Debug("pr-review-agent start", slog.String("component", "pr-review-agent"))
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Fprintln(os.Stdout, version.String())
	}
}
