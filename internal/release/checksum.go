package release

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// ChecksumsName is the SHA-256 manifest each release publishes
// (.goreleaser.yaml checksum.name_template).
const ChecksumsName = "checksums.txt"

// ParseChecksums reads sha256sum output: "<hex>  <name>" per line.
func ParseChecksums(text string) (map[string]string, error) {
	sums := map[string]string{}
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 2 {
			return nil, fmt.Errorf("bad checksum line %q", line)
		}
		sums[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
	}
	return sums, nil
}

// VerifyChecksum checks data against name's entry in sums. A file with no
// entry fails: an unlisted download is as untrusted as a tampered one.
func VerifyChecksum(data []byte, name string, sums map[string]string) error {
	want, ok := sums[name]
	if !ok {
		return fmt.Errorf("no checksum for %s in %s", name, ChecksumsName)
	}
	h := sha256.Sum256(data)
	if got := hex.EncodeToString(h[:]); got != want {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s (the download is corrupt or tampered; nothing was installed)", name, got, want)
	}
	return nil
}
