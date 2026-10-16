package store

import (
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestLegacyPasswordHashUpgradesOnLogin(t *testing.T) {
	st := newStore(t)
	// Install a legacy salted-SHA256 hash and confirm a login upgrades it.
	if err := st.UpdateSettings(func(s *Settings) {
		s.AdminSalt = "cafe"
		s.AdminPassHash = legacyHash("admin", "cafe")
	}); err != nil {
		t.Fatal(err)
	}
	if !isLegacyHash(st.Settings().AdminPassHash) {
		t.Fatal("setup failed: hash is not legacy")
	}
	if !st.CheckAdmin("admin", "admin") {
		t.Fatal("legacy login rejected")
	}
	if isLegacyHash(st.Settings().AdminPassHash) {
		t.Fatal("expected the legacy hash to be upgraded after a successful login")
	}
	if !st.CheckAdmin("admin", "admin") {
		t.Fatal("login rejected after hash upgrade")
	}
	if st.CheckAdmin("admin", "wrong") {
		t.Fatal("wrong password accepted")
	}
}

func TestSetAdminPasswordBumpsEpoch(t *testing.T) {
	st := newStore(t)
	before := st.Settings().SessionEpoch
	if err := st.SetAdminPassword("admin", "another-secret"); err != nil {
		t.Fatal(err)
	}
	if st.Settings().SessionEpoch == before {
		t.Fatal("password change did not bump the session epoch")
	}
	if !st.CheckAdmin("admin", "another-secret") {
		t.Fatal("new password rejected")
	}
}

func TestRecoveryCodeConsumedOnce(t *testing.T) {
	st := newStore(t)
	code := "ABCDE-12345"
	hash := st.HashRecoveryCode(code)
	if err := st.ReplaceRecoveryHashes([]string{hash}); err != nil {
		t.Fatal(err)
	}
	if h := st.HashRecoveryCode("abcde 12345"); h != hash {
		t.Fatal("normalisation mismatch: same code hashed differently")
	}
	if !st.ConsumeRecoveryHash(hash) {
		t.Fatal("recovery code not recognised")
	}
	if st.ConsumeRecoveryHash(hash) {
		t.Fatal("recovery code accepted twice")
	}
}

func TestTwoFAToggle(t *testing.T) {
	st := newStore(t)
	if err := st.EnableTwoFA("SECRET", []string{"h1", "h2"}); err != nil {
		t.Fatal(err)
	}
	set := st.Settings()
	if !set.TwoFAEnabled || set.TwoFASecret != "SECRET" || len(set.RecoveryHashes) != 2 {
		t.Fatalf("unexpected settings after enable: %+v", set)
	}
	if err := st.DisableTwoFA(); err != nil {
		t.Fatal(err)
	}
	set = st.Settings()
	if set.TwoFAEnabled || set.TwoFASecret != "" || len(set.RecoveryHashes) != 0 {
		t.Fatalf("unexpected settings after disable: %+v", set)
	}
}

func TestNodeByToken(t *testing.T) {
	st := newStore(t)
	n, err := st.CreateNode("web-1", "host1", "linux", "amd64", "v1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.NodeByToken(n.Token)
	if err != nil {
		t.Fatalf("NodeByToken: %v", err)
	}
	if got.ID != n.ID {
		t.Fatalf("got node %s, want %s", got.ID, n.ID)
	}
	if _, err := st.NodeByToken("nope"); err != ErrNotFound {
		t.Fatalf("unknown token err = %v, want ErrNotFound", err)
	}
	if _, err := st.NodeByToken(""); err != ErrNotFound {
		t.Fatalf("empty token err = %v, want ErrNotFound", err)
	}
}

func TestNodeConnectionIP(t *testing.T) {
	st := newStore(t)
	n, err := st.CreateNode("web-1", "host1", "linux", "amd64", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeIP(n.ID, "10.1.2.3"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetNode(n.ID); got.IP != "10.1.2.3" {
		t.Fatalf("IP after SetNodeIP = %q, want 10.1.2.3", got.IP)
	}
	// An empty IP must not clobber a previously recorded address.
	if err := st.SetNodeIP(n.ID, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetNode(n.ID); got.IP != "10.1.2.3" {
		t.Fatalf("IP after empty SetNodeIP = %q, want 10.1.2.3", got.IP)
	}
	// A heartbeat also refreshes the IP and the reported version.
	if err := st.TouchNode(n.ID, "host1", "linux", "amd64", "v2", "10.9.9.9"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetNode(n.ID)
	if got.IP != "10.9.9.9" {
		t.Fatalf("IP after TouchNode = %q, want 10.9.9.9", got.IP)
	}
	if got.AgentVersion != "v2" {
		t.Fatalf("version = %q, want v2", got.AgentVersion)
	}
}
