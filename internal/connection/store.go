// Package connection persists the credentials owned by the homelab web
// service. Unlike credstore, which only reads other CLIs' files, this package
// owns refresh-token rotation and writes an encrypted application state.
package connection

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/valche5/ai-usage/internal/provider"
)

const envelopeMagic = "AIU1"

// Connection is one provider credential. Access and Refresh are secrets and
// must never be returned by an HTTP handler or included in an error.
type Connection struct {
	ID        string    `json:"id,omitempty"` // unique storage key; empty preserves legacy single-account entries
	Provider  string    `json:"provider"`
	Kind      string    `json:"kind"` // oauth or api
	Access    string    `json:"access"`
	Refresh   string    `json:"refresh,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	AccountID string    `json:"account_id,omitempty"`
	Email     string    `json:"email,omitempty"`
	Plan      string    `json:"plan,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// StorageID is the unique key of this credential. Existing encrypted stores
// omitted ID and therefore keep their historical provider key until migrated.
func (c Connection) StorageID() string {
	if c.ID != "" {
		return c.ID
	}
	return c.Provider
}

type state struct {
	Connections map[string]Connection      `json:"connections"`
	Reports     map[string]provider.Report `json:"reports,omitempty"`
}

// Store encrypts the complete state with AES-256-GCM and writes it atomically.
// It is deliberately file-backed: this service is single-user and single-
// instance, so a database would add operational weight without adding safety.
type Store struct {
	mu   sync.RWMutex
	path string
	aead cipher.AEAD
	data state
}

// Open initializes a store in dir. key must be a base64-encoded 32-byte key.
func Open(dir, key string) (*Store, error) {
	raw, err := decodeKey(key)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, fmt.Errorf("initialiser le chiffrement: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialiser AES-GCM: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("créer le répertoire de données: %w", err)
	}
	s := &Store{
		path: filepath.Join(dir, "state.enc"),
		aead: aead,
		data: state{Connections: map[string]Connection{}, Reports: map[string]provider.Report{}},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func decodeKey(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("AI_USAGE_ENCRYPTION_KEY est requis (génère-le avec `openssl rand -base64 32`)")
	}
	var raw []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		raw, err = enc.DecodeString(value)
		if err == nil {
			break
		}
	}
	if err != nil || len(raw) != 32 {
		return nil, errors.New("AI_USAGE_ENCRYPTION_KEY doit être une clé de 32 octets encodée en base64")
	}
	return raw, nil
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lire le stockage chiffré: %w", err)
	}
	if len(b) < len(envelopeMagic)+s.aead.NonceSize() || string(b[:len(envelopeMagic)]) != envelopeMagic {
		return errors.New("stockage chiffré invalide")
	}
	nonce := b[len(envelopeMagic) : len(envelopeMagic)+s.aead.NonceSize()]
	ciphertext := b[len(envelopeMagic)+s.aead.NonceSize():]
	plain, err := s.aead.Open(nil, nonce, ciphertext, []byte(envelopeMagic))
	if err != nil {
		return errors.New("impossible de déchiffrer state.enc (mauvaise clé ou fichier altéré)")
	}
	if err := json.Unmarshal(plain, &s.data); err != nil {
		return errors.New("contenu de state.enc invalide")
	}
	if s.data.Connections == nil {
		s.data.Connections = map[string]Connection{}
	}
	if s.data.Reports == nil {
		s.data.Reports = map[string]provider.Report{}
	}
	return nil
}

func (s *Store) persistLocked() error {
	plain, err := json.Marshal(s.data)
	if err != nil {
		return fmt.Errorf("encoder l'état: %w", err)
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("générer le nonce: %w", err)
	}
	out := append([]byte(envelopeMagic), nonce...)
	out = s.aead.Seal(out, nonce, plain, []byte(envelopeMagic))
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return fmt.Errorf("écrire le stockage chiffré: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("publier le stockage chiffré: %w", err)
	}
	return nil
}

// Get returns a copy of one connection.
func (s *Store) Get(id string) (Connection, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.data.Connections[id]
	return c, ok
}

// IDs returns connected provider IDs without exposing their secrets.
func (s *Store) IDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.data.Connections))
	for id := range s.data.Connections {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (s *Store) Put(c Connection) error {
	id := c.StorageID()
	if id == "" || c.Provider == "" || c.Access == "" {
		return errors.New("connexion incomplète")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c.UpdatedAt = time.Now().UTC()
	s.data.Connections[id] = c
	return s.persistLocked()
}

// Replace stores a newly authorized credential and invalidates the report that
// belonged to the previous account. Refresh rotation uses Put instead, because
// it must retain the stale fallback until the next collection succeeds.
func (s *Store) Replace(c Connection) error {
	id := c.StorageID()
	if id == "" || c.Provider == "" || c.Access == "" {
		return errors.New("connexion incomplète")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c.UpdatedAt = time.Now().UTC()
	s.data.Connections[id] = c
	delete(s.data.Reports, id)
	return s.persistLocked()
}

// Rekey atomically moves a connection and its cached report to a stable ID.
func (s *Store) Rekey(oldID string, c Connection) error {
	newID := c.StorageID()
	if oldID == "" || newID == "" || c.Provider == "" || c.Access == "" {
		return errors.New("migration de connexion incomplète")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data.Connections[oldID]; !ok {
		return nil
	}
	c.UpdatedAt = time.Now().UTC()
	delete(s.data.Connections, oldID)
	s.data.Connections[newID] = c
	if report, ok := s.data.Reports[oldID]; ok {
		delete(s.data.Reports, oldID)
		report.ID = newID
		s.data.Reports[newID] = report
	}
	return s.persistLocked()
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Connections, id)
	delete(s.data.Reports, id)
	return s.persistLocked()
}

// Reports returns copies of the last normalized, secret-free reports.
func (s *Store) Reports() map[string]provider.Report {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]provider.Report, len(s.data.Reports))
	for id, report := range s.data.Reports {
		report.TokenFP = ""
		report.CredPath = ""
		out[id] = report
	}
	return out
}

func (s *Store) PutReports(reports []provider.Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, report := range reports {
		if _, connected := s.data.Connections[report.ID]; !connected {
			continue
		}
		report.TokenFP = ""
		report.CredPath = ""
		s.data.Reports[report.ID] = report
	}
	return s.persistLocked()
}
