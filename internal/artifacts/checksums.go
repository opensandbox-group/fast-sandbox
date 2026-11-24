package artifacts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
)

// ParseSHA256SUMS reads bundle expectations without trusting the files being checked.
// Callers must obtain the manifest from an operator-controlled deployment bundle.
func ParseSHA256SUMS(raw []byte) (map[string]string, error) {
	entries := make(map[string]string)
	for number, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("SHA256SUMS line %d is malformed", number+1)
		}
		sum, err := hex.DecodeString(fields[0])
		name := strings.TrimPrefix(strings.TrimPrefix(fields[1], "*"), "./")
		if err != nil {
			return nil, fmt.Errorf("SHA256SUMS line %d: bad digest: %w", number+1, err)
		}
		if len(sum) != sha256.Size || fields[0] != strings.ToLower(fields[0]) {
			return nil, fmt.Errorf("SHA256SUMS line %d: digest must be 64 lowercase hex characters", number+1)
		}
		if !filepath.IsLocal(name) || filepath.Clean(name) != name {
			return nil, fmt.Errorf("SHA256SUMS line %d: path %q must be local and clean", number+1, name)
		}
		if _, exists := entries[name]; exists {
			return nil, fmt.Errorf("SHA256SUMS duplicates %s", name)
		}
		entries[name] = fields[0]
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("SHA256SUMS lists no entries")
	}
	return entries, nil
}
