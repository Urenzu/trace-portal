// Package identity resolves what the capturing process stamps onto every event.
//
// It is the seam the login flow will write to. Today nothing has logged in, so
// the answer is a local enrollment minted on first run; once the device-
// authorization flow exists, `trace-portal login` opens a browser, the server
// returns a tenant, a user and a collector token, and they land in this same
// file. Nothing downstream of here knows or cares which of the two happened —
// the ids are opaque either way, which is the whole point of minting them
// ourselves rather than writing an identity provider's subject onto a turn.
package identity

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Urenzu/trace-portal/internal/trace"
)

// enrollmentFile sits beside the archive it describes. Moving a data directory
// to another machine therefore carries its identity with it, which is correct:
// the turns in it were produced by that user, wherever the bytes now live.
const enrollmentFile = "identity.json"

// LocalTenant and LocalUser are the ids used before anyone has logged in.
//
// They are real values rather than empty strings on purpose. An unenrolled
// archive is still attributed — to the local account — so there is never a
// population of turns with no owner to reconcile later. When a login arrives,
// these are what the enrollment upgrades *from*, and a migration knows exactly
// which rows it is claiming.
const (
	LocalTenant = "local"
	LocalUser   = "local"
)

// Enrollment is what this installation knows about who it is capturing for.
type Enrollment struct {
	TenantID  string `json:"tenant_id"`
	UserID    string `json:"user_id"`
	MachineID string `json:"machine_id"`

	// Token is the collector credential the server issues at login. It is
	// absent locally, and it is the only secret this file ever holds — which is
	// why the file is written 0600 rather than 0644 like the checkpoint beside
	// it.
	Token string `json:"token,omitempty"`

	// Server is where batches are shipped. Empty means local-only capture: the
	// archive stays on this machine and nothing is sent anywhere.
	Server string `json:"server,omitempty"`
}

// Identity is the subset stamped onto events. The token and server address are
// deliberately not part of it: a credential must never reach the archive.
func (e Enrollment) Identity() trace.Identity {
	return trace.Identity{TenantID: e.TenantID, UserID: e.UserID, MachineID: e.MachineID}
}

// Local reports whether this installation is still the unenrolled single-user
// case, which is what the UI needs to know before offering to sign in.
func (e Enrollment) Local() bool {
	return e.TenantID == LocalTenant && e.UserID == LocalUser
}

// Load reads the enrollment under dataDir, creating a local one on first run.
//
// Environment overrides exist so several identities can be exercised against
// one build before the login flow is written; they are a development affordance
// and are ignored once a token is present, since a real enrollment must not be
// silently re-pointed at another tenant by a stray variable.
func Load(dataDir string) (Enrollment, error) {
	path := filepath.Join(dataDir, enrollmentFile)

	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		var e Enrollment
		if err := json.Unmarshal(raw, &e); err != nil {
			return Enrollment{}, fmt.Errorf("decode %s: %w", enrollmentFile, err)
		}
		if e.MachineID == "" {
			// An enrollment written by a build that did not mint one, or one
			// hand-edited. A machine id is cheap to add and unlike the other two
			// it is genuinely local knowledge, so fill it and persist.
			if e.MachineID, err = newID(); err != nil {
				return Enrollment{}, err
			}
			if err := Save(dataDir, e); err != nil {
				return Enrollment{}, err
			}
		}
		if e.Token == "" {
			applyOverrides(&e)
		}
		if err := e.validate(); err != nil {
			return Enrollment{}, err
		}
		return e, nil

	case errors.Is(err, os.ErrNotExist):
		machine, err := newID()
		if err != nil {
			return Enrollment{}, err
		}
		e := Enrollment{TenantID: LocalTenant, UserID: LocalUser, MachineID: machine}
		applyOverrides(&e)
		if err := e.validate(); err != nil {
			return Enrollment{}, err
		}
		return e, Save(dataDir, e)

	default:
		return Enrollment{}, fmt.Errorf("read %s: %w", enrollmentFile, err)
	}
}

// Save writes the enrollment atomically. The login flow calls this; so does the
// first run that mints a local one.
//
// Creates the data directory. This is the first thing in the process to touch
// it — the enrollment is loaded before the store is opened, because the store
// needs the identity it will stamp — so it cannot assume something else has
// made it. Pointing the tool at a fresh directory otherwise failed with a
// path-not-found on a temporary file, which reads as a bug in the archive
// rather than as "that directory does not exist yet".
func Save(dataDir string, e Enrollment) error {
	if err := e.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	raw, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dataDir, enrollmentFile)
	tmp := path + ".tmp"
	// 0600: this is the only file in the archive that can hold a credential.
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func applyOverrides(e *Enrollment) {
	if v := strings.TrimSpace(os.Getenv("TRACE_PORTAL_TENANT")); v != "" {
		e.TenantID = v
	}
	if v := strings.TrimSpace(os.Getenv("TRACE_PORTAL_USER")); v != "" {
		e.UserID = v
	}
}

// validate refuses an enrollment that cannot attribute a turn. Capturing
// unattributed data is the one failure this whole package exists to prevent, so
// it is an error rather than a warning: better to refuse to start than to write
// a day of turns nobody owns.
func (e Enrollment) validate() error {
	if e.TenantID == "" || e.UserID == "" {
		return errors.New("enrollment has no tenant or user; refusing to capture unattributed turns")
	}
	return nil
}

// newID mints an opaque 128-bit identifier.
//
// Random rather than derived from the hostname or the OS username: those name
// the operator, and this tool spends real effort elsewhere not to record such
// things. A random id is stable across a hostname change, which a derived one
// would not be, and it discloses nothing if the archive is ever shipped.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
