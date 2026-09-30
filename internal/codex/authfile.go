package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// AuthFile mirrors the $CODEX_HOME/auth.json layout the Codex CLI reads for ChatGPT logins.
type AuthFile struct {
	AuthMode     string     `json:"auth_mode,omitempty"`
	OpenAIAPIKey *string    `json:"OPENAI_API_KEY"`
	Tokens       *Tokens    `json:"tokens,omitempty"`
	LastRefresh  *time.Time `json:"last_refresh,omitempty"`
}

// Home returns the Codex config directory: $CODEX_HOME, or ~/.codex.
func Home() (string, error) {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h, nil
	}
	u, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the home directory: %w", err)
	}
	return filepath.Join(u, ".codex"), nil
}

// AuthPath is the auth.json path inside home.
func AuthPath(home string) string { return filepath.Join(home, "auth.json") }

// ReadAuthFile reads auth.json. A missing file returns (nil, nil): no one is logged in.
func ReadAuthFile(path string) (*AuthFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	// Windows editors (Notepad, PowerShell 5.1 Set-Content -Encoding utf8) prepend a UTF-8 BOM.
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	var f AuthFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s is not a valid auth.json", path)
	}
	return &f, nil
}

// NewChatGPTAuthFile builds the auth.json content for a ChatGPT login.
func NewChatGPTAuthFile(t Tokens, lastRefresh time.Time) *AuthFile {
	lr := lastRefresh.UTC()
	return &AuthFile{AuthMode: "chatgpt", Tokens: &t, LastRefresh: &lr}
}

// WriteAuthFile writes auth.json atomically with owner-only permissions: a temp file in the same
// directory is renamed over the target, so a running Codex never reads a half-written file.
func WriteAuthFile(path string, f *AuthFile) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("encode auth.json: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".auth-*.tmp")
	if err != nil {
		return fmt.Errorf("create a temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("restrict the temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return fmt.Errorf("write the temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close the temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
