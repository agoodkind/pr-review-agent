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
	event := strings.Split(payload, ":")[1]
	slog.Info("decoded webhook", "event", event)
	fmt.Println(event)
}
