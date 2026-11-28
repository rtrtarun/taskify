package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zalando/go-keyring"
)

const keychainService = "taskctl"

// Where a credential is kept, in the order tried: the operating system
// keychain (Windows Credential Manager, macOS Keychain, the Linux Secret
// Service), then a file only the user can read. The token is never written
// to config.yaml, never taken from a flag, and never printed.
type credStore struct {
	keychain keychain
	file     string // path of the fallback file
}

// keychain is the part of go-keyring the store uses, so tests can replace it.
type keychain interface {
	Get(service, user string) (string, error)
	Set(service, user, password string) error
	Delete(service, user string) error
}

type osKeychain struct{}

func (osKeychain) Get(s, u string) (string, error) { return keyring.Get(s, u) }
func (osKeychain) Set(s, u, p string) error        { return keyring.Set(s, u, p) }
func (osKeychain) Delete(s, u string) error        { return keyring.Delete(s, u) }

// Save stores the profile's token and says where it went: "keychain" or
// "file". A token in one place is removed from the other, so a stale copy
// cannot outlive a login.
func (c credStore) Save(profile, token string) (string, error) {
	if err := c.keychain.Set(keychainService, profile, token); err == nil {
		_ = c.removeFromFile(profile)
		return "keychain", nil
	}
	// No keychain here (a headless Linux box, say): use the file.
	if err := c.writeFile(profile, token); err != nil {
		return "", fmt.Errorf("no keychain is available and the credentials file could not be written: %w", err)
	}
	return "file", nil
}

// Load returns the profile's token, or "" and "" when there is none.
func (c credStore) Load(profile string) (token, where string, err error) {
	if t, kerr := c.keychain.Get(keychainService, profile); kerr == nil && t != "" {
		return t, "keychain", nil
	}
	m, err := c.readFile()
	if err != nil {
		return "", "", err
	}
	if t := m[profile]; t != "" {
		return t, "file", nil
	}
	return "", "", nil
}

func (c credStore) Delete(profile string) error {
	_ = c.keychain.Delete(keychainService, profile)
	return c.removeFromFile(profile)
}

func (c credStore) readFile() (map[string]string, error) {
	if err := checkPrivate(c.file); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	raw, err := os.ReadFile(c.file)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(raw, &m); err != nil {
		// Do not wrap err: a decoder error may quote the file's content.
		return nil, fmt.Errorf("%s is not a credentials file", c.file)
	}
	return m, nil
}

func (c credStore) writeFile(profile, token string) error {
	m, err := c.readFile()
	if err != nil {
		return err
	}
	if token == "" {
		delete(m, profile)
	} else {
		m[profile] = token
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.file), 0o700); err != nil {
		return err
	}
	// Write a private temporary file, then rename it over the real one, so
	// the token is never in a file anyone else could read, even briefly.
	tmp := c.file + ".tmp"
	_ = os.Remove(tmp) // a leftover file may not be private
	f, err := createPrivate(tmp)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, c.file)
}

func (c credStore) removeFromFile(profile string) error {
	if _, err := os.Stat(c.file); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return c.writeFile(profile, "")
}

// Redaction. Output goes to stderr through redactWriter, which replaces any
// token this run knows, anything shaped like a JWT, and anything after the
// word "Bearer". Nothing prints a token on purpose; this catches an error
// from a library that quotes one.
var (
	jwtShape    = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`)
	bearerShape = regexp.MustCompile(`(?i)(bearer)\s+\S+`)
)

const redacted = "[redacted]"

type redactor struct{ secrets []string }

func (r *redactor) add(token string) {
	if token != "" {
		r.secrets = append(r.secrets, token)
	}
}

func (r *redactor) clean(s string) string {
	for _, sec := range r.secrets {
		s = strings.ReplaceAll(s, sec, redacted)
	}
	s = jwtShape.ReplaceAllString(s, redacted)
	return bearerShape.ReplaceAllString(s, "$1 "+redacted)
}
