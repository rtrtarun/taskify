package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCredentialsPreferTheKeychain(t *testing.T) {
	kc := &fakeKeychain{m: map[string]string{}}
	file := filepath.Join(t.TempDir(), "credentials.json")
	st := credStore{keychain: kc, file: file}
	where, err := st.Save("dev", "tok-1")
	if err != nil || where != "keychain" {
		t.Fatalf("Save = %q, %v", where, err)
	}
	if _, err := os.Stat(file); err == nil {
		t.Error("a credentials file was written although the keychain worked")
	}
	if tok, w, _ := st.Load("dev"); tok != "tok-1" || w != "keychain" {
		t.Errorf("Load = %q from %q", tok, w)
	}
	if tok, _, _ := st.Load("other"); tok != "" {
		t.Errorf("another profile's token: %q", tok)
	}
	if err := st.Delete("dev"); err != nil {
		t.Fatal(err)
	}
	if tok, _, _ := st.Load("dev"); tok != "" {
		t.Error("the token survived Delete")
	}
}

func TestFallbackFileIsPrivate(t *testing.T) {
	kc := &fakeKeychain{m: map[string]string{}, fail: true}
	dir := filepath.Join(t.TempDir(), "sub")
	file := filepath.Join(dir, "credentials.json")
	st := credStore{keychain: kc, file: file}

	where, err := st.Save("dev", "tok-secret-1")
	if err != nil || where != "file" {
		t.Fatalf("Save = %q, %v", where, err)
	}
	if _, err := st.Save("ci", "tok-secret-2"); err != nil {
		t.Fatal(err)
	}
	if tok, w, err := st.Load("dev"); err != nil || tok != "tok-secret-1" || w != "file" {
		t.Errorf("Load dev = %q from %q, %v", tok, w, err)
	}
	if _, err := os.Stat(file + ".tmp"); err == nil {
		t.Error("the temporary file was left behind")
	}
	assertOwnerOnly(t, file)

	if err := st.Delete("dev"); err != nil {
		t.Fatal(err)
	}
	if tok, _, _ := st.Load("dev"); tok != "" {
		t.Error("dev survived Delete")
	}
	if tok, _, _ := st.Load("ci"); tok != "tok-secret-2" {
		t.Error("Delete removed another profile")
	}
}

func assertOwnerOnly(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("mode %04o lets others read the file", fi.Mode().Perm())
		}
		return
	}
	// Windows: the ACL must have no inherited entries and name only the
	// current user (icacls prints one line per entry, indented).
	out, err := exec.Command("icacls", path).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls: %v: %s", err, out)
	}
	var entries []string
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, path) {
			l = strings.TrimSpace(strings.TrimPrefix(l, path))
		}
		if strings.Contains(l, ":(") {
			entries = append(entries, l)
		}
	}
	if len(entries) != 1 {
		t.Errorf("want exactly one ACL entry, got %q", entries)
	}
	for _, e := range entries {
		if strings.Contains(e, "(I)") {
			t.Errorf("inherited ACL entry: %q", e)
		}
		for _, broad := range []string{"Everyone", "BUILTIN\\Users", "Authenticated Users"} {
			if strings.Contains(e, broad) {
				t.Errorf("ACL entry for %s: %q", broad, e)
			}
		}
	}
}

func TestWorldReadableFileIsRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits do not restrict access on Windows; the ACL is set when the file is created")
	}
	file := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(file, []byte(`{"dev":"tok-secret"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	st := credStore{keychain: &fakeKeychain{m: map[string]string{}, fail: true}, file: file}
	_, _, err := st.Load("dev")
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("Load = %v", err)
	}
	if strings.Contains(err.Error(), "tok-secret") {
		t.Error("the error quotes the token")
	}
}

func TestCorruptFileErrorDoesNotQuoteIt(t *testing.T) {
	file := filepath.Join(t.TempDir(), "credentials.json")
	f, err := createPrivate(file)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"dev":"tok-secret"`)
	_ = f.Close()
	st := credStore{keychain: &fakeKeychain{m: map[string]string{}, fail: true}, file: file}
	if _, _, err := st.Load("dev"); err == nil || strings.Contains(err.Error(), "tok-secret") {
		t.Fatalf("Load = %v", err)
	}
}

func TestRedactor(t *testing.T) {
	r := &redactor{}
	r.add("plain-token-xyz")
	for in, banned := range map[string]string{
		"failed with plain-token-xyz here":                        "plain-token-xyz",
		"Authorization: Bearer abc.def.ghi":                       "abc.def.ghi",
		"token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1In0.c2lnbmF0dXJl": "eyJhbGci",
		`bearer "quoted"`:                                         "",
	} {
		got := r.clean(in)
		if banned != "" && strings.Contains(got, banned) {
			t.Errorf("clean(%q) = %q", in, got)
		}
		if !strings.Contains(got, redacted) {
			t.Errorf("clean(%q) = %q, nothing redacted", in, got)
		}
	}
}
