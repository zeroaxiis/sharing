package config

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/zeroaxiis/sharing/daemon/protocol"
)

// TrustFileName is the paired-device store inside AppDir.
const TrustFileName = "trusted.json"

// ErrNoSuchPeer is returned when an operation names a device that is not in the
// store.
var ErrNoSuchPeer = errors.New("trust: no such peer")

// TrustedPeer is one device this machine has paired with. Field names are the
// exact JSON keys written to trusted.json.
//
// Token is a shared secret: 32 bytes of crypto/rand, hex-encoded, issued by
// whichever side acted as responder during pairing and stored identically on
// both. Anything that can read it can impersonate the peer, which is why the
// file is 0600 inside a 0700 directory and why comparisons go through
// VerifyToken.
type TrustedPeer struct {
	DeviceID string    `json:"deviceId"`
	Name     string    `json:"name"`
	Token    string    `json:"token"`
	PairedAt time.Time `json:"pairedAt"`
	LastSeen time.Time `json:"lastSeen"`
}

// trustFile is the on-disk envelope: {"peers":[...]}.
type trustFile struct {
	Peers []TrustedPeer `json:"peers"`
}

// TrustStore is the set of devices this machine has paired with, backed by
// trusted.json.
//
// It is read on every inbound peer connection and written during pairing, from
// different goroutines, so every method takes the lock. Reads use RLock so a
// burst of inbound connections does not serialise behind each other.
type TrustStore struct {
	log  *slog.Logger
	path string

	mu    sync.RWMutex
	peers map[string]TrustedPeer

	// writeMu serialises persistence. It is separate from mu so a slow disk
	// never blocks a VerifyToken on an inbound connection, and so two
	// concurrent writers cannot interleave their renames.
	writeMu sync.Mutex
}

// TrustPath returns the absolute path of trusted.json.
func TrustPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, TrustFileName), nil
}

// LoadTrustStore opens the trust store at the default location, creating
// nothing until the first write.
func LoadTrustStore(logger *slog.Logger) (*TrustStore, error) {
	path, err := TrustPath()
	if err != nil {
		return nil, err
	}
	return LoadTrustStoreAt(logger, path)
}

// LoadTrustStoreAt opens the trust store at an explicit path. Tests use this to
// stay out of the real user config directory.
//
// A missing file yields an empty store. A corrupt file also yields an empty
// store: it is logged at warn level and moved aside to <path>.corrupt rather
// than failing the daemon's boot. Refusing to start would take the whole
// product down over a file whose worst-case loss is "the user pairs again",
// and would hand a local attacker a trivial denial of service.
func LoadTrustStoreAt(logger *slog.Logger, path string) (*TrustStore, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if path == "" {
		return nil, errors.New("trust: store path is empty")
	}

	s := &TrustStore{
		log:   logger,
		path:  path,
		peers: make(map[string]TrustedPeer),
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		// Parsed below.
	case errors.Is(err, os.ErrNotExist):
		logger.Debug("no trust store yet, starting empty", "path", path)
		return s, nil
	default:
		logger.Warn("trust store unreadable, starting empty", "path", path, "error", err)
		return s, nil
	}

	var file trustFile
	if err := json.Unmarshal(data, &file); err != nil {
		logger.Warn("trust store is corrupt, starting empty; existing pairings must be redone",
			"path", path, "error", err)
		s.quarantine(data)
		return s, nil
	}

	skipped := 0
	for _, p := range file.Peers {
		// A record without an id cannot be looked up, and one without a
		// well-formed token can never authenticate anything. Dropping them
		// keeps VerifyToken from ever comparing against garbage.
		if p.DeviceID == "" || !protocol.ValidTokenFormat(p.Token) {
			skipped++
			continue
		}
		s.peers[p.DeviceID] = p
	}
	if skipped > 0 {
		logger.Warn("dropped unusable entries from the trust store", "path", path, "count", skipped)
	}

	logger.Info("loaded trust store", "path", path, "peers", len(s.peers))
	return s, nil
}

// Path is the file this store persists to.
func (s *TrustStore) Path() string { return s.path }

// Len reports how many peers are trusted.
func (s *TrustStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.peers)
}

// Get returns the stored record for deviceID.
func (s *TrustStore) Get(deviceID string) (TrustedPeer, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.peers[deviceID]
	return p, ok
}

// IsPaired reports whether deviceID is trusted. It says nothing about whether
// the peer presented a valid token; use VerifyToken for that.
func (s *TrustStore) IsPaired(deviceID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.peers[deviceID]
	return ok
}

// List returns every trusted peer, sorted by name then device id so the UI gets
// a stable order across calls.
func (s *TrustStore) List() []TrustedPeer {
	s.mu.RLock()
	out := make([]TrustedPeer, 0, len(s.peers))
	for _, p := range s.peers {
		out = append(out, p)
	}
	s.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].DeviceID < out[j].DeviceID
	})
	return out
}

// PairedIDs returns the trusted device ids as a set, for annotating a discovery
// snapshot with Device.Paired without a lock acquisition per device.
func (s *TrustStore) PairedIDs() map[string]struct{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]struct{}, len(s.peers))
	for id := range s.peers {
		out[id] = struct{}{}
	}
	return out
}

// Add records a successful pairing and persists the store, replacing any
// previous record for the same device. PairedAt and LastSeen are set to now:
// re-pairing is a fresh trust decision by a human, not a continuation of the
// old one.
//
// The token must be TokenHexLen lowercase hex, as produced by NewToken.
func (s *TrustStore) Add(deviceID, name, token string) (TrustedPeer, error) {
	if deviceID == "" {
		return TrustedPeer{}, errors.New("trust: deviceId is required")
	}
	if !protocol.ValidTokenFormat(token) {
		return TrustedPeer{}, fmt.Errorf("trust: token must be %d lowercase hex characters", protocol.TokenHexLen)
	}

	now := time.Now().UTC().Truncate(time.Second)
	peer := TrustedPeer{
		DeviceID: deviceID,
		Name:     name,
		Token:    token,
		PairedAt: now,
		LastSeen: now,
	}

	// writeMu before mu, everywhere: it makes "snapshot then persist" one
	// indivisible step, so two concurrent pairings cannot rename their files in
	// the opposite order and leave the older snapshot on disk.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	s.peers[deviceID] = peer
	snapshot := s.snapshotLocked()
	s.mu.Unlock()

	if err := s.writeLocked(snapshot); err != nil {
		return TrustedPeer{}, err
	}
	s.log.Info("paired device recorded", "deviceId", deviceID, "name", name)
	return peer, nil
}

// Remove forgets a device and persists the store. It reports whether the device
// was present; removing an unknown device is not an error, so a UI can send
// pair:forget without racing another client.
func (s *TrustStore) Remove(deviceID string) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	_, existed := s.peers[deviceID]
	if existed {
		delete(s.peers, deviceID)
	}
	snapshot := s.snapshotLocked()
	s.mu.Unlock()

	if !existed {
		return false, nil
	}
	if err := s.writeLocked(snapshot); err != nil {
		return false, err
	}
	s.log.Info("forgot paired device", "deviceId", deviceID)
	return true, nil
}

// Touch updates LastSeen for a trusted peer and persists the store. Returns
// ErrNoSuchPeer if the device is not trusted.
//
// Call this on a successful handshake, not on every frame: it writes the file.
func (s *TrustStore) Touch(deviceID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	peer, ok := s.peers[deviceID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNoSuchPeer, deviceID)
	}
	peer.LastSeen = time.Now().UTC().Truncate(time.Second)
	s.peers[deviceID] = peer
	snapshot := s.snapshotLocked()
	s.mu.Unlock()

	return s.writeLocked(snapshot)
}

// Rename updates the stored display name for a trusted peer, so a device that
// was renamed since pairing does not show up under its old name forever.
func (s *TrustStore) Rename(deviceID, name string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	peer, ok := s.peers[deviceID]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrNoSuchPeer, deviceID)
	}
	if peer.Name == name {
		s.mu.Unlock()
		return nil
	}
	peer.Name = name
	s.peers[deviceID] = peer
	snapshot := s.snapshotLocked()
	s.mu.Unlock()

	return s.writeLocked(snapshot)
}

// VerifyToken reports whether token is the credential stored for deviceID.
//
// The comparison is crypto/subtle.ConstantTimeCompare and never ==. A byte-wise
// comparison that returns early leaks how many leading characters were right,
// and an attacker on the LAN can measure that over enough connections to
// recover a 64-character token one character at a time. An unknown device is
// still compared, against a fixed dummy of the same length, so the "no such
// peer" path takes the same shape as a wrong token.
func (s *TrustStore) VerifyToken(deviceID, token string) bool {
	s.mu.RLock()
	peer, ok := s.peers[deviceID]
	s.mu.RUnlock()

	want := peer.Token
	if !ok {
		// Not a secret and never accepted: the length match only keeps the
		// timing of this branch close to the real one.
		want = dummyToken
	}

	equal := subtle.ConstantTimeCompare([]byte(want), []byte(token)) == 1
	return ok && equal
}

// dummyToken is a well-formed but unreachable token used to keep the
// unknown-device path in VerifyToken the same shape as the known-device path.
// It is never stored and never issued.
var dummyToken = func() string {
	b := make([]byte, protocol.TokenBytes)
	return hex.EncodeToString(b)
}()

// TokensEqual compares two tokens in constant time. Exposed for callers that
// hold a token outside the store, such as a dialer checking the token it just
// received against the one it already had.
func TokensEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// NewToken mints a pairing token: protocol.TokenBytes bytes from crypto/rand,
// hex-encoded to protocol.TokenHexLen lowercase characters.
//
// crypto/rand, never math/rand: a predictable token is a free pass onto this
// machine for anyone who can reach the peer port.
func NewToken() (string, error) {
	buf := make([]byte, protocol.TokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate pairing token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// -----------------------------------------------------------------------------
// Persistence
// -----------------------------------------------------------------------------

// snapshotLocked copies the current peers into a stable, sorted file envelope.
// Caller must hold the write lock.
func (s *TrustStore) snapshotLocked() trustFile {
	peers := make([]TrustedPeer, 0, len(s.peers))
	for _, p := range s.peers {
		peers = append(peers, p)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].DeviceID < peers[j].DeviceID })
	return trustFile{Peers: peers}
}

// writeLocked persists a snapshot atomically: a temp file in the same directory, then
// a rename. A crash mid-write can therefore lose the newest pairing but can
// never leave a truncated trusted.json that locks every paired device out.
//
// The caller must hold writeMu and must NOT hold mu: disk I/O is slow and
// inbound connections doing VerifyToken must not queue behind it.
func (s *TrustStore) writeLocked(file trustFile) error {
	if file.Peers == nil {
		file.Peers = []TrustedPeer{}
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode trust store: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("create config dir %s: %w", dir, err)
	}

	// Temp file in the destination directory so the rename stays on one
	// filesystem and is therefore atomic.
	tmp, err := os.CreateTemp(dir, TrustFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temp trust store in %s: %w", dir, err)
	}
	tmpName := tmp.Name()

	cleanup := func() {
		tmp.Close()
		if rmErr := os.Remove(tmpName); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			s.log.Debug("remove temp trust store", "path", tmpName, "error", rmErr)
		}
	}

	// os.CreateTemp already makes the file 0600, but assert it rather than rely
	// on it: the token is a secret and this is cheap.
	if err := tmp.Chmod(filePerm); err != nil && !errors.Is(err, os.ErrInvalid) {
		// Chmod is a no-op on some platforms; only a real failure matters.
		s.log.Debug("chmod temp trust store", "path", tmpName, "error", err)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temp trust store %s: %w", tmpName, err)
	}
	// Flush to disk before the rename, so the rename cannot land ahead of the
	// bytes and expose an empty file after a power loss.
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temp trust store %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		if rmErr := os.Remove(tmpName); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			s.log.Debug("remove temp trust store", "path", tmpName, "error", rmErr)
		}
		return fmt.Errorf("close temp trust store %s: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, s.path); err != nil {
		if rmErr := os.Remove(tmpName); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			s.log.Debug("remove temp trust store", "path", tmpName, "error", rmErr)
		}
		return fmt.Errorf("rename trust store into place: %w", err)
	}

	if err := os.Chmod(s.path, filePerm); err != nil {
		return fmt.Errorf("chmod trust store %s: %w", s.path, err)
	}
	return nil
}

// quarantine preserves an unparseable trust store next to the original so a
// user can recover it by hand, instead of having it silently overwritten by the
// next successful pairing. Best effort: a failure here must not stop boot.
func (s *TrustStore) quarantine(data []byte) {
	dest := s.path + ".corrupt"
	if err := os.WriteFile(dest, data, filePerm); err != nil {
		s.log.Debug("could not preserve corrupt trust store", "path", dest, "error", err)
		return
	}
	s.log.Warn("preserved the corrupt trust store for inspection", "path", dest)
}
