package pool

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const deployTokensFilename = "deploy-tokens.json"

var fallbackDeployTokenMu sync.Mutex

type LegacyDeployToken struct {
	App   string
	Label string
	Token string
}

type DeployTokenMetadata struct {
	ID         string     `json:"id"`
	App        string     `json:"app"`
	Label      string     `json:"label"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	Legacy     bool       `json:"legacy"`
}

type deployTokenRecord struct {
	DeployTokenMetadata
	Digest string `json:"digest"`
}

type deployTokenFile struct {
	Version        int                 `json:"version"`
	LegacyImported bool                `json:"legacyImported"`
	Tokens         []deployTokenRecord `json:"tokens"`
}

func (s Store) InitializeDeployTokens(legacy []LegacyDeployToken) error {
	mu := s.deployTokenMutex()
	mu.Lock()
	defer mu.Unlock()

	path := filepath.Join(s.dir, deployTokensFilename)
	if _, err := os.Stat(path); err == nil {
		_, err = s.loadDeployTokenFile()
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect deploy token store: %w", err)
	}

	now := time.Now().UTC()
	file := deployTokenFile{Version: 1, LegacyImported: true}
	seen := make(map[string]bool)
	for _, item := range legacy {
		app := strings.TrimSpace(item.App)
		token := strings.TrimSpace(item.Token)
		if app == "" || token == "" {
			continue
		}
		if !safeID(app) {
			return fmt.Errorf("legacy deploy token contains invalid app name %q", app)
		}
		if len(token) < 32 {
			return fmt.Errorf("legacy deploy token for app %q must be at least 32 characters", app)
		}
		digest := deployTokenDigest(token)
		if key := app + "\x00" + digest; seen[key] {
			continue
		} else {
			seen[key] = true
		}
		id, err := randomDeployTokenPart(9)
		if err != nil {
			return err
		}
		label := normalizeDeployTokenLabel(item.Label)
		file.Tokens = append(file.Tokens, deployTokenRecord{
			DeployTokenMetadata: DeployTokenMetadata{ID: id, App: app, Label: label, CreatedAt: now, Legacy: true},
			Digest:              digest,
		})
	}
	return s.writeDeployTokenFile(file)
}

func (s Store) MintDeployToken(app, label string) (string, DeployTokenMetadata, error) {
	mu := s.deployTokenMutex()
	mu.Lock()
	defer mu.Unlock()

	cfg, _, err := s.Load()
	if err != nil {
		return "", DeployTokenMetadata{}, err
	}
	if _, ok := cfg.FindApp(app); !ok {
		return "", DeployTokenMetadata{}, fmt.Errorf("unknown app %q", app)
	}
	file, err := s.loadDeployTokenFile()
	if err != nil {
		return "", DeployTokenMetadata{}, err
	}
	if len(file.Tokens) >= 256 {
		return "", DeployTokenMetadata{}, errors.New("deploy token store limit reached")
	}
	appCount := 0
	for _, record := range file.Tokens {
		if record.App == app {
			appCount++
		}
	}
	if appCount >= 32 {
		return "", DeployTokenMetadata{}, fmt.Errorf("app %q already has the maximum 32 deploy tokens", app)
	}

	var id string
	for {
		id, err = randomDeployTokenPart(9)
		if err != nil {
			return "", DeployTokenMetadata{}, err
		}
		collision := false
		for _, record := range file.Tokens {
			collision = collision || record.ID == id
		}
		if !collision {
			break
		}
	}
	secret, err := randomDeployTokenPart(32)
	if err != nil {
		return "", DeployTokenMetadata{}, err
	}
	plaintext := "poolctl_v1." + id + "." + secret
	metadata := DeployTokenMetadata{
		ID: id, App: app, Label: normalizeDeployTokenLabel(label), CreatedAt: time.Now().UTC(),
	}
	file.Tokens = append(file.Tokens, deployTokenRecord{DeployTokenMetadata: metadata, Digest: deployTokenDigest(plaintext)})
	if err := s.writeDeployTokenFile(file); err != nil {
		return "", DeployTokenMetadata{}, err
	}
	return plaintext, metadata, nil
}

func (s Store) ListDeployTokens(app string) ([]DeployTokenMetadata, error) {
	mu := s.deployTokenMutex()
	mu.Lock()
	defer mu.Unlock()
	file, err := s.loadDeployTokenFile()
	if err != nil {
		return nil, err
	}
	result := make([]DeployTokenMetadata, 0)
	for _, record := range file.Tokens {
		if record.App == app {
			result = append(result, record.DeployTokenMetadata)
		}
	}
	return result, nil
}

func (s Store) AuthorizeDeployToken(app, plaintext string) (bool, error) {
	mu := s.deployTokenMutex()
	mu.Lock()
	defer mu.Unlock()
	file, err := s.loadDeployTokenFile()
	if err != nil {
		return false, err
	}
	digest := deployTokenDigest(plaintext)
	matched := -1
	for i, record := range file.Tokens {
		appMatches := subtle.ConstantTimeCompare([]byte(record.App), []byte(app)) == 1
		digestMatches := subtle.ConstantTimeCompare([]byte(record.Digest), []byte(digest)) == 1
		if appMatches && digestMatches {
			matched = i
		}
	}
	if matched < 0 {
		return false, nil
	}
	now := time.Now().UTC()
	file.Tokens[matched].LastUsedAt = &now
	if err := s.writeDeployTokenFile(file); err != nil {
		return false, err
	}
	return true, nil
}

func (s Store) RevokeDeployToken(app, id string) error {
	mu := s.deployTokenMutex()
	mu.Lock()
	defer mu.Unlock()
	file, err := s.loadDeployTokenFile()
	if err != nil {
		return err
	}
	filtered := file.Tokens[:0]
	found := false
	for _, record := range file.Tokens {
		if record.App == app && record.ID == id {
			found = true
			continue
		}
		filtered = append(filtered, record)
	}
	if !found {
		return fmt.Errorf("unknown deploy token %q for app %q", id, app)
	}
	file.Tokens = filtered
	return s.writeDeployTokenFile(file)
}

func (s Store) RemoveDeployTokens(app string) error {
	mu := s.deployTokenMutex()
	mu.Lock()
	defer mu.Unlock()
	if _, err := os.Stat(filepath.Join(s.dir, deployTokensFilename)); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect deploy token store: %w", err)
	}
	file, err := s.loadDeployTokenFile()
	if err != nil {
		return err
	}
	filtered := file.Tokens[:0]
	for _, record := range file.Tokens {
		if record.App != app {
			filtered = append(filtered, record)
		}
	}
	file.Tokens = filtered
	return s.writeDeployTokenFile(file)
}

func (s Store) loadDeployTokenFile() (deployTokenFile, error) {
	raw, err := os.ReadFile(filepath.Join(s.dir, deployTokensFilename))
	if errors.Is(err, os.ErrNotExist) {
		return deployTokenFile{Version: 1, LegacyImported: true}, nil
	}
	if err != nil {
		return deployTokenFile{}, fmt.Errorf("read deploy token store: %w", err)
	}
	var file deployTokenFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return deployTokenFile{}, fmt.Errorf("parse deploy token store: %w", err)
	}
	if file.Version != 1 || !file.LegacyImported {
		return deployTokenFile{}, errors.New("unsupported or incomplete deploy token store")
	}
	for _, record := range file.Tokens {
		if !safeID(record.App) || record.ID == "" || len(record.Digest) != sha256.Size*2 {
			return deployTokenFile{}, errors.New("deploy token store contains an invalid record")
		}
		if _, err := hex.DecodeString(record.Digest); err != nil {
			return deployTokenFile{}, errors.New("deploy token store contains an invalid digest")
		}
	}
	return file, nil
}

func (s Store) writeDeployTokenFile(file deployTokenFile) error {
	file.Version = 1
	file.LegacyImported = true
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encode deploy token store: %w", err)
	}
	raw = append(raw, '\n')
	temporary, err := os.CreateTemp(s.dir, ".deploy-tokens-*")
	if err != nil {
		return fmt.Errorf("create temporary deploy token store: %w", err)
	}
	temporaryName := temporary.Name()
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary deploy token store: %w", err)
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary deploy token store: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary deploy token store: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary deploy token store: %w", err)
	}
	if err := os.Rename(temporaryName, filepath.Join(s.dir, deployTokensFilename)); err != nil {
		return fmt.Errorf("replace deploy token store: %w", err)
	}
	removeTemporary = false
	return nil
}

func (s Store) deployTokenMutex() *sync.Mutex {
	if s.tokenMu != nil {
		return s.tokenMu
	}
	return &fallbackDeployTokenMu
}

func randomDeployTokenPart(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate deploy token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func deployTokenDigest(plaintext string) string {
	digest := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(digest[:])
}

func normalizeDeployTokenLabel(label string) string {
	label = strings.Join(strings.Fields(label), " ")
	if label == "" {
		return "CI deploy token"
	}
	if len(label) > 80 {
		return label[:80]
	}
	return label
}
