// Command lifecycle-proof provides a disposable review fixture.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

func main() {
	payload := strings.TrimSpace(os.Getenv("WEBHOOK_PAYLOAD"))
	_, event, found := strings.Cut(payload, ":")
	if !found {
		slog.Info("ignored malformed webhook")
		return
	}
	slog.Info("decoded webhook", "event", event)
	fmt.Println(event)
}
