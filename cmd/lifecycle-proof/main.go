// Command lifecycle-proof provides a disposable review fixture.
package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

func main() {
	label := strings.ToLower(strings.TrimSpace(os.Getenv("LABEL")))
	slog.Info("normalized label", "label", label)
	fmt.Println(label)
}
