package identity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadMintsLocalEnrollmentOnFirstRun(t *testing.T) {
	dir := t.TempDir()

	e, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !e.Local() {
		t.Fatalf("first run should be local, got tenant=%q user=%q", e.TenantID, e.UserID)
	}
	if !e.Identity().Attributed() {
		t.Fatal("a freshly minted enrollment must still attribute its turns")
	}
	if len(e.MachineID) != 32 {
		t.Fatalf("machine id should be 128 random bits as hex, got %q", e.MachineID)
	}
}

// The machine id has to survive a restart. If it were re-minted each run, one
// laptop would look like a new machine every time the process started, and the
// deduplication it exists to support would have nothing stable to key on.
func TestMachineIDIsStableAcrossLoads(t *testing.T) {
	dir := t.TempDir()

	first, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	second, err := Load(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if first.MachineID != second.MachineID {
		t.Fatalf("machine id changed across loads: %q then %q", first.MachineID, second.MachineID)
	}
}

// Two installations must not collide, or one tenant's turns would deduplicate
// against another's.
func TestMachineIDDiffersBetweenInstallations(t *testing.T) {
	a, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("load a: %v", err)
	}
	b, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("load b: %v", err)
	}
	if a.MachineID == b.MachineID {
		t.Fatal("two installations minted the same machine id")
	}
}

// The token is the only secret in the archive. A world-readable file holding it
// is a real leak on a shared machine, so the mode is asserted rather than
// assumed. Windows does not model POSIX permissions, so the check is skipped
// there rather than made meaningless.
func TestEnrollmentFileIsNotWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not enforced on this platform")
	}
	dir := t.TempDir()
	if err := Save(dir, Enrollment{TenantID: "t_1", UserID: "u_1", MachineID: "m_1", Token: "secret"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, enrollmentFile))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Fatalf("enrollment file is %v; it can hold a token and must be owner-only", mode)
	}
}

// An enrollment that cannot attribute a turn is refused rather than accepted
// and worked around downstream. Capturing unattributed data is the one outcome
// this package exists to prevent, and it is unrepairable after the fact.
func TestUnattributedEnrollmentIsRefused(t *testing.T) {
	dir := t.TempDir()
	raw, _ := json.Marshal(Enrollment{MachineID: "m_1"})
	if err := os.WriteFile(filepath.Join(dir, enrollmentFile), raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("an enrollment with no tenant or user should not load")
	}
}

// The development overrides exist so several identities can be exercised before
// the login flow is written. They must not touch a real enrollment: a stray
// variable in a shell must never re-point an authenticated collector at another
// tenant.
func TestOverridesApplyLocallyAndAreIgnoredOnceEnrolled(t *testing.T) {
	t.Setenv("TRACE_PORTAL_TENANT", "t_override")
	t.Setenv("TRACE_PORTAL_USER", "u_override")

	local, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("load local: %v", err)
	}
	if local.TenantID != "t_override" || local.UserID != "u_override" {
		t.Fatalf("overrides ignored on an unenrolled install: %+v", local)
	}

	enrolled := t.TempDir()
	if err := Save(enrolled, Enrollment{TenantID: "t_real", UserID: "u_real", MachineID: "m_1", Token: "tok"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := Load(enrolled)
	if err != nil {
		t.Fatalf("load enrolled: %v", err)
	}
	if got.TenantID != "t_real" || got.UserID != "u_real" {
		t.Fatalf("environment re-pointed an enrolled collector: %+v", got)
	}
}

// Pointing the tool at a directory that does not exist yet must work. The
// enrollment is loaded before the store is opened -- the store needs the
// identity it will stamp -- so nothing else has created the directory by then.
func TestLoadCreatesTheDataDirectory(t *testing.T) {
	fresh := filepath.Join(t.TempDir(), "does", "not", "exist", "yet")

	e, err := Load(fresh)
	if err != nil {
		t.Fatalf("load into a fresh directory: %v", err)
	}
	if !e.Local() || e.MachineID == "" {
		t.Fatalf("incomplete enrollment: %+v", e)
	}
	if _, err := os.Stat(filepath.Join(fresh, enrollmentFile)); err != nil {
		t.Fatalf("enrollment was not persisted: %v", err)
	}
}
