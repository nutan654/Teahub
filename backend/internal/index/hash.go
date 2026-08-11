package index

import (
	"crypto/sha256"
	"encoding/hex"
)

// hashContent returns a stable content hash used to detect whether a file
// actually changed between an initial Build and a later Update — this is
// what lets Update skip re-analyzing files whose content is byte-identical
// to what's already indexed.
func hashContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}
