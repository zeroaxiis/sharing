package config

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zeroaxiis/sharing/daemon/protocol"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestStore(t *testing.T) *TrustStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), TrustFileName)
	s, err := LoadTrustStoreAt(quietLogger(), path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestNewTokenShape(t *testing.T) {
	seen := make(map[string]struct{}, 32)
	for i := 0; i < 32; i++ {
		tok, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != protocol.TokenHexLen || !protocol.ValidTokenFormat(tok) {
			t.Fatalf("token %q is not %d lowercase hex characters", tok, protocol.TokenHexLen)
		}
		if _, dup := seen[tok]; dup {
			t.Fatalf("NewToken repeated a value: %q", tok)
		}
		seen[tok] = struct{}{}
	}
}

func TestAddGetListRemove(t *testing.T) {
	s := newTestStore(t)
	tok, _ := NewToken()

	if s.IsPaired("dev-a") {
		t.Fatal("empty store reports a pairing")
	}

	peer, err := s.Add("dev-a", "Their Laptop", tok)
	if err != nil {
		t.Fatal(err)
	}
	if peer.PairedAt.IsZero() || peer.LastSeen.IsZero() {
		t.Fatalf("timestamps not set: %+v", peer)
	}

	got, ok := s.Get("dev-a")
	if !ok || got.Token != tok || got.Name != "Their Laptop" {
		t.Fatalf("Get = %+v %v", got, ok)
	}
	if !s.IsPaired("dev-a") || s.Len() != 1 {
		t.Fatalf("IsPaired/Len wrong: %v %d", s.IsPaired("dev-a"), s.Len())
	}
	if ids := s.PairedIDs(); len(ids) != 1 {
		t.Fatalf("PairedIDs = %v", ids)
	}

	other, _ := NewToken()
	if _, err := s.Add("dev-b", "Another", other); err != nil {
		t.Fatal(err)
	}
	list := s.List()
	if len(list) != 2 || list[0].Name != "Another" {
		t.Fatalf("List not sorted by name: %+v", list)
	}

	removed, err := s.Remove("dev-a")
	if err != nil || !removed {
		t.Fatalf("Remove = %v %v", removed, err)
	}
	if removed, err := s.Remove("dev-a"); err != nil || removed {
		t.Fatalf("second Remove = %v %v", removed, err)
	}
	if s.IsPaired("dev-a") || s.Len() != 1 {
		t.Fatal("device still trusted after Remove")
	}
}

func TestAddRejectsBadInput(t *testing.T) {
	s := newTestStore(t)
	tok, _ := NewToken()

	if _, err := s.Add("", "No Id", tok); err == nil {
		t.Fatal("Add accepted an empty deviceId")
	}
	for _, bad := range []string{"", "deadbeef", strings.ToUpper(tok)} {
		if _, err := s.Add("dev-a", "Bad Token", bad); err == nil {
			t.Fatalf("Add accepted a malformed token: %q", bad)
		}
	}
}

func TestVerifyToken(t *testing.T) {
	s := newTestStore(t)
	tok, _ := NewToken()
	if _, err := s.Add("dev-a", "Their Laptop", tok); err != nil {
		t.Fatal(err)
	}

	if !s.VerifyToken("dev-a", tok) {
		t.Fatal("correct token rejected")
	}
	// One flipped character must fail.
	wrong := []byte(tok)
	if wrong[0] == '0' {
		wrong[0] = '1'
	} else {
		wrong[0] = '0'
	}
	if s.VerifyToken("dev-a", string(wrong)) {
		t.Fatal("a token differing in one character was accepted")
	}
	// A correct prefix must not help.
	if s.VerifyToken("dev-a", tok[:len(tok)-1]) {
		t.Fatal("a truncated token was accepted")
	}
	if s.VerifyToken("dev-a", "") {
		t.Fatal("an empty token was accepted")
	}
	if s.VerifyToken("unknown-device", tok) {
		t.Fatal("an unknown device was accepted")
	}
	if !TokensEqual(tok, tok) || TokensEqual(tok, "x") {
		t.Fatal("TokensEqual is wrong")
	}
}

func TestPersistAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, TrustFileName)

	s, err := LoadTrustStoreAt(quietLogger(), path)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := NewToken()
	if _, err := s.Add("dev-a", "Their Laptop", tok); err != nil {
		t.Fatal(err)
	}
	if err := s.Touch("dev-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Rename("dev-a", "Renamed Laptop"); err != nil {
		t.Fatal(err)
	}
	if err := s.Touch("nope"); err == nil {
		t.Fatal("Touch on an unknown device did not error")
	}

	// The file must be the exact envelope the spec names, with camelCase keys.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Peers []map[string]any `json:"peers"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.Peers) != 1 {
		t.Fatalf("peers = %+v", probe.Peers)
	}
	for _, key := range []string{"deviceId", "name", "token", "pairedAt", "lastSeen"} {
		if _, ok := probe.Peers[0][key]; !ok {
			t.Fatalf("missing key %q in %+v", key, probe.Peers[0])
		}
	}

	// No temp files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != TrustFileName {
		t.Fatalf("unexpected files in the config dir: %v", entries)
	}

	reloaded, err := LoadTrustStoreAt(quietLogger(), path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := reloaded.Get("dev-a")
	if !ok || got.Token != tok || got.Name != "Renamed Laptop" {
		t.Fatalf("reloaded = %+v %v", got, ok)
	}
	if !reloaded.VerifyToken("dev-a", tok) {
		t.Fatal("token did not survive a reload")
	}
}

func TestCorruptFileStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, TrustFileName)
	if err := os.WriteFile(path, []byte("{not json at all"), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := LoadTrustStoreAt(quietLogger(), path)
	if err != nil {
		t.Fatalf("a corrupt trust store must not fail the boot: %v", err)
	}
	if s.Len() != 0 {
		t.Fatalf("expected an empty store, got %d peers", s.Len())
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("corrupt file was not preserved: %v", err)
	}

	// The store must still be usable afterwards.
	tok, _ := NewToken()
	if _, err := s.Add("dev-a", "Fresh", tok); err != nil {
		t.Fatal(err)
	}
}

func TestUnusableEntriesAreDropped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, TrustFileName)
	good, _ := NewToken()
	body := fmt.Sprintf(`{"peers":[
      {"deviceId":"","name":"no id","token":%q,"pairedAt":"2026-01-01T00:00:00Z","lastSeen":"2026-01-01T00:00:00Z"},
      {"deviceId":"dev-b","name":"short token","token":"abcd","pairedAt":"2026-01-01T00:00:00Z","lastSeen":"2026-01-01T00:00:00Z"},
      {"deviceId":"dev-c","name":"fine","token":%q,"pairedAt":"2026-01-01T00:00:00Z","lastSeen":"2026-01-01T00:00:00Z"}
    ]}`, good, good)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := LoadTrustStoreAt(quietLogger(), path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 1 || !s.IsPaired("dev-c") {
		t.Fatalf("expected only dev-c to survive, got %+v", s.List())
	}
}

// The store is read on every inbound peer connection and written during
// pairing. Run with -race: this must not report anything.
func TestConcurrentAccess(t *testing.T) {
	s := newTestStore(t)
	tok, _ := NewToken()
	if _, err := s.Add("dev-a", "Their Laptop", tok); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.VerifyToken("dev-a", tok)
				s.List()
				s.PairedIDs()
				s.IsPaired("dev-a")
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := fmt.Sprintf("dev-%d", n)
			for j := 0; j < 25; j++ {
				other, err := NewToken()
				if err != nil {
					t.Error(err)
					return
				}
				if _, err := s.Add(id, "Churn", other); err != nil {
					t.Error(err)
					return
				}
				if _, err := s.Remove(id); err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	wg.Wait()

	if !s.VerifyToken("dev-a", tok) {
		t.Fatal("the untouched pairing did not survive the churn")
	}
}
