package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// homeDir is where uplink keeps its token, event log and transcripts.
func homeDir() (string, error) {
	if dir := os.Getenv("UPLINK_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate home directory: %w (set UPLINK_HOME)", err)
	}
	return filepath.Join(home, ".uplink"), nil
}

func tokenPath() (string, error) {
	dir, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "token"), nil
}

// loadToken resolves the operator token.
//
// Order: UPLINK_TOKEN, then --token-file, then ~/.uplink/token. A token is never
// accepted on the command line: argv is world-readable through ps, and this
// token authorises command execution on every crew.
func loadToken(explicitFile string, createIfMissing bool) (string, error) {
	if tok := strings.TrimSpace(os.Getenv("UPLINK_TOKEN")); tok != "" {
		return tok, nil
	}

	path := explicitFile
	if path == "" {
		var err error
		if path, err = tokenPath(); err != nil {
			return "", err
		}
	}

	data, err := os.ReadFile(path)
	if err == nil {
		tok := strings.TrimSpace(string(data))
		if tok == "" {
			return "", fmt.Errorf("token file %s is empty", path)
		}
		return tok, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	if !createIfMissing {
		// Naming --create matters: it is a flag on the command the user most
		// likely just ran. Creating one stays opt-in, because a stray
		// `uplink token` on a crew host would mint a token that does not match
		// ground's, and the resulting 401s are a confusing way to find out.
		return "", fmt.Errorf("no token found at %s.\n"+
			"  On the machine running ground:  uplink init\n"+
			"  On a crew host:                 export UPLINK_TOKEN=<the token from the ground machine>",
			path)
	}

	tok, err := generateToken(path)
	if err != nil {
		return "", err
	}
	return tok, nil
}

// generateToken writes a fresh 32-byte token with owner-only permissions.
func generateToken(path string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(buf[:])
	// O_EXCL so a concurrent start cannot clobber a token already in use.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create token file: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(tok + "\n"); err != nil {
		return "", err
	}
	return tok, nil
}

// groundURL resolves the ground base URL.
func groundURL(flagValue string) string {
	if flagValue != "" {
		return strings.TrimRight(flagValue, "/")
	}
	if env := os.Getenv("UPLINK_GROUND"); env != "" {
		return strings.TrimRight(env, "/")
	}
	return "http://127.0.0.1:8765"
}

// execLookPath reports whether a binary is reachable on PATH.
func execLookPath(name string) (string, error) { return exec.LookPath(name) }
