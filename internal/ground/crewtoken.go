package ground

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// CrewTokenPrefix marks a crew credential. It exists so an error can say "that
// is a crew token, this endpoint wants the operator token" without echoing the
// secret back at whoever sent it.
const CrewTokenPrefix = "uplc_"

// IsCrewToken reports whether a presented secret looks like a crew token.
func IsCrewToken(token string) bool {
	return strings.HasPrefix(token, CrewTokenPrefix)
}

// CrewToken is one crew's credential, stored as a hash.
type CrewToken struct {
	Name string `json:"name"`
	// Roles, when set, are authoritative: a crew cannot claim a role its token
	// does not grant. Empty means the crew declares its own.
	Roles     []string   `json:"roles,omitempty"`
	Hash      string     `json:"hash"` // sha256 of the token, hex
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// Revoked reports whether this credential has been withdrawn.
func (t *CrewToken) Revoked() bool { return t.RevokedAt != nil }

// crewTokenFile is the on-disk shape, versioned so the format can move later.
type crewTokenFile struct {
	Version int          `json:"version"`
	Tokens  []*CrewToken `json:"tokens"`
}

// CrewTokenStore holds the crew credentials.
//
// The CLI is the only writer and ground is the only reader, so there is no
// coordination to do beyond writing atomically. Ground re-reads the file when it
// changes, which is what makes a freshly minted token work — and a revoked one
// stop working — without restarting the daemon.
type CrewTokenStore struct {
	path string

	mu     sync.Mutex
	byHash map[string]*CrewToken
	order  []string // names, in mint order
	byName map[string]*CrewToken
	mtime  time.Time
	size   int64
	loaded bool
}

// OpenCrewTokens prepares the store at path. A missing file is not an error:
// nothing has been minted yet, and crew auth will say so.
func OpenCrewTokens(path string) (*CrewTokenStore, error) {
	s := &CrewTokenStore{path: path}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.loadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path returns the file backing this store.
func (s *CrewTokenStore) Path() string { return s.path }

func (s *CrewTokenStore) loadLocked() error {
	s.byHash = map[string]*CrewToken{}
	s.byName = map[string]*CrewToken{}
	s.order = nil
	s.loaded = true

	info, err := os.Stat(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.mtime, s.size = time.Time{}, 0
			return nil
		}
		return err
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var file crewTokenFile
	if len(data) > 0 {
		if err := json.Unmarshal(data, &file); err != nil {
			return fmt.Errorf("parse %s: %w", s.path, err)
		}
	}
	for _, t := range file.Tokens {
		if t == nil || t.Name == "" || t.Hash == "" {
			continue
		}
		s.byHash[t.Hash] = t
		if _, dup := s.byName[t.Name]; !dup {
			s.order = append(s.order, t.Name)
		}
		s.byName[t.Name] = t
	}
	s.mtime, s.size = info.ModTime(), info.Size()
	return nil
}

// reloadIfChangedLocked re-reads the file when it has been written since the
// last look. One stat per crew authentication is nothing at this request volume.
func (s *CrewTokenStore) reloadIfChangedLocked() {
	info, err := os.Stat(s.path)
	switch {
	case err != nil && os.IsNotExist(err):
		if !s.mtime.IsZero() || len(s.byHash) > 0 {
			_ = s.loadLocked() // the file was removed: forget everything
		}
		return
	case err != nil:
		return // keep serving what we have
	}
	if info.ModTime().Equal(s.mtime) && info.Size() == s.size {
		return
	}
	_ = s.loadLocked()
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Lookup resolves a presented secret to its crew credential. It reports the
// token even when revoked, so the caller can say which of the two it is.
func (s *CrewTokenStore) Lookup(presented string) (*CrewToken, bool) {
	if presented == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadIfChangedLocked()
	// Keyed on the hash, so this is a plain map lookup: an attacker would need
	// the preimage, and there is no secret-dependent comparison to time.
	t, ok := s.byHash[hashToken(presented)]
	return t, ok
}

// Count reports how many credentials exist, revoked included.
func (s *CrewTokenStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadIfChangedLocked()
	return len(s.byHash)
}

// List returns the credentials in mint order, without their hashes.
func (s *CrewTokenStore) List() []CrewToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadIfChangedLocked()
	out := make([]CrewToken, 0, len(s.order))
	for _, name := range s.order {
		t := s.byName[name]
		copied := *t
		copied.Hash = ""
		out = append(out, copied)
	}
	return out
}

// Mint issues a new credential for a crew name and returns it once. The
// plaintext is never stored, so it cannot be shown again.
func (s *CrewTokenStore) Mint(name string, roles []string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("crew name is required")
	}
	if strings.ContainsAny(name, " \t\n") {
		return "", fmt.Errorf("crew name must not contain whitespace")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadIfChangedLocked()

	if existing, ok := s.byName[name]; ok && !existing.Revoked() {
		return "", fmt.Errorf("crew %q already has a token; revoke it first: uplink crew-token revoke %s", name, name)
	}

	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	plaintext := CrewTokenPrefix + hex.EncodeToString(buf[:])

	token := &CrewToken{
		Name:      name,
		Roles:     roles,
		Hash:      hashToken(plaintext),
		CreatedAt: time.Now().UTC(),
	}
	// Replacing a revoked entry keeps the file from growing a row per rotation.
	if _, seen := s.byName[name]; !seen {
		s.order = append(s.order, name)
	} else {
		delete(s.byHash, s.byName[name].Hash)
	}
	s.byName[name] = token
	s.byHash[token.Hash] = token

	if err := s.saveLocked(); err != nil {
		return "", err
	}
	return plaintext, nil
}

// Revoke withdraws a crew's credential. It takes effect on that crew's next
// request, with no ground restart.
func (s *CrewTokenStore) Revoke(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadIfChangedLocked()

	t, ok := s.byName[name]
	if !ok {
		return fmt.Errorf("no crew token named %q", name)
	}
	if t.Revoked() {
		return fmt.Errorf("crew token %q was already revoked", name)
	}
	now := time.Now().UTC()
	t.RevokedAt = &now
	return s.saveLocked()
}

// saveLocked writes the file atomically, so a reader never sees a partial one.
func (s *CrewTokenStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}

	tokens := make([]*CrewToken, 0, len(s.order))
	for _, name := range s.order {
		tokens = append(tokens, s.byName[name])
	}
	sort.SliceStable(tokens, func(i, j int) bool { return tokens[i].CreatedAt.Before(tokens[j].CreatedAt) })

	data, err := json.MarshalIndent(crewTokenFile{Version: 1, Tokens: tokens}, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".crew-tokens-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}

	// Record what we just wrote so the next Lookup does not reload needlessly.
	if info, err := os.Stat(s.path); err == nil {
		s.mtime, s.size = info.ModTime(), info.Size()
	}
	return nil
}
