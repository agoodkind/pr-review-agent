package review

import (
	"crypto/sha256"
	"encoding/hex"

	"goodkind.io/pr-review-agent/internal/diff"
)

func chunkID(chunk diff.Chunk) string {
	digest := sha256.Sum256([]byte(chunk.Text))
	return hex.EncodeToString(digest[:])[:chunkIDLength]
}

func removeChunkID(pending []string, id string) []string {
	remaining := make([]string, 0, len(pending))
	for _, item := range pending {
		if item == id {
			continue
		}
		remaining = append(remaining, item)
	}
	return remaining
}
