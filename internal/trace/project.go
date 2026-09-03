package trace

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// projectIDLength is enough to keep collisions implausible at any number of
// projects a person has, while staying short enough to read in a URL.
const projectIDLength = 12

// Project reduces an absolute working directory to a readable name.
//
// The full path is deliberately discarded. On a typical machine it reads
// C:\Users\<name>\dev\projects\<project>, which names the operator and every
// project alongside it — detail this tool has no reason to keep and every
// reason not to transmit.
func Project(dir string) string {
	dir = strings.TrimRight(strings.TrimSpace(dir), `/\`)
	if dir == "" {
		return ""
	}
	// Split on both separators explicitly rather than with path/filepath,
	// whose separator is the *server's* OS -- a transcript can carry a
	// Windows path onto a Linux server, or the reverse, and either style
	// must reduce the same way regardless of where this code runs.
	base := dir[strings.LastIndexAny(dir, `/\`)+1:]
	// A drive or filesystem root reduces to nothing meaningful.
	if base == "" || strings.HasSuffix(base, ":") {
		return ""
	}
	return base
}

// ProjectID is a stable, one-way identifier for a working directory. Two
// projects sharing a folder name stay distinct, and the path itself cannot be
// recovered from it.
func ProjectID(dir string) string {
	dir = strings.TrimRight(strings.TrimSpace(dir), `/\`)
	if dir == "" {
		return ""
	}
	// Normalize separators and case so the same project keeps one id whether it
	// was recorded from a shell using forward slashes or a native Windows path.
	// Explicit, not path/filepath.ToSlash, for the same cross-OS reason as
	// Project above.
	norm := strings.ToLower(strings.NewReplacer(`\`, "/").Replace(dir))
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])[:projectIDLength]
}
