package trace

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
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
	base := filepath.Base(filepath.FromSlash(dir))
	// A drive or filesystem root reduces to nothing meaningful.
	if base == "." || base == string(filepath.Separator) || strings.HasSuffix(base, ":") {
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
	norm := strings.ToLower(filepath.ToSlash(dir))
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])[:projectIDLength]
}
