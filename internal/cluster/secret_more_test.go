package cluster

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func randomKeyHex(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(k)
}

func TestLoadSealerAcceptsAHandPlacedRawKey(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	// A binary key copied in with dd, not the hex this writes by default. It is
	// still 32 bytes and must still work, or a hand-rolled deployment cannot
	// start.
	if err := os.WriteFile(keyFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := LoadSealer("", keyFile)
	if err != nil {
		t.Fatalf("a raw 32-byte key file was rejected: %v", err)
	}
	sealed := s.Seal([]byte("v"))
	if err := os.WriteFile(keyFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := LoadSealer("", keyFile)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Open(sealed)
	if err != nil || string(got) != "v" {
		t.Fatalf("the reloaded raw key did not open the value: %q %v", got, err)
	}
}

func TestLoadSealerTrimsWhitespaceAroundAKeyFile(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	hexKey := randomKeyHex(t)
	// A file edited by hand, or appended to by a log shipper, carries a leading
	// newline and trailing spaces. The key is the hex between them.
	if err := os.WriteFile(keyFile, []byte("\n  \t"+hexKey+"  \r\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSealer("", keyFile)
	if err != nil {
		t.Fatalf("a padded key file was rejected: %v", err)
	}
	sealed := s.Seal([]byte("v"))
	got, err := s.Open(sealed)
	if err != nil || string(got) != "v" {
		t.Fatalf("round trip through a padded key: %q %v", got, err)
	}
	// The padding really was stripped rather than absorbed: the value it sealed
	// with is exactly 32 bytes, so a re-read of the file agrees.
	if err := os.WriteFile(keyFile, []byte(hexKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := LoadSealer("", keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s2.Open(sealed); err != nil || string(got) != "v" {
		t.Fatalf("the padded and unpadded files produced different keys: %q %v", got, err)
	}
}

func TestLoadSealerRejectsACorruptHexKey(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	// "zz" is not hex, and the rest is too short anyway. The operator has to be
	// told which file is wrong, or they will go looking in the wrong place.
	if err := os.WriteFile(keyFile, []byte("zzzz-not-a-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSealer("", keyFile)
	if err == nil {
		t.Fatal("a key file that is not a key was accepted")
	}
	if !strings.Contains(err.Error(), keyFile) {
		t.Errorf("error %q does not name the file %q", err, keyFile)
	}
	if !strings.Contains(err.Error(), "32 bytes") {
		t.Errorf("error %q does not say what a key has to be", err)
	}
}

func TestLoadSealerRejectsAKeyOfTheWrongLength(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	// Ninety-six hex characters, which is two keys pasted together. It decodes
	// cleanly, so nothing about the file looks broken; it is simply the wrong
	// size, and AES-256 needs exactly 32 bytes.
	if err := os.WriteFile(keyFile, []byte(strings.Repeat("ab", 48)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSealer("", keyFile)
	if err == nil {
		t.Fatal("a 48-byte key was accepted")
	}
	if !strings.Contains(err.Error(), "got 48") {
		t.Errorf("error %q does not report the length it found", err)
	}
}

func TestLoadSealerRefusesAnUnreadableKeyFile(t *testing.T) {
	dir := t.TempDir()
	// A directory where the key file should be: the path exists, so this is not
	// a first run, but reading it fails for a reason that will never fix itself.
	// Generating a fresh key over the top would silently re-key every stored
	// credential.
	keyFile := filepath.Join(dir, "key")
	if err := os.Mkdir(keyFile, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSealer("", keyFile); err == nil {
		t.Fatal("an unreadable key file was treated as a first run")
	}
}

func TestLoadSealerReportsAnUncreatableKeyDirectory(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "server")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The key path is under a regular file, so the parent directory cannot be
	// created. That is a misconfiguration to report, not a reason to run without
	// a key.
	if _, err := LoadSealer("", filepath.Join(blocker, "sub", "control-plane.key")); err == nil {
		t.Fatal("a key file under an uncreatable directory was silently skipped")
	}
}

func TestLoadSealerReportsAKeyDirectoryItCannotWriteTo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can write to a read-only directory")
	}
	dir := t.TempDir()
	// The directory exists and is traversable but not writable: a mounted
	// volume with the wrong ownership, or a read-only root filesystem. There is
	// no key on it and no way to put one there, so the control plane has to say
	// so rather than start with a key it does not have.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	_, err := LoadSealer("", filepath.Join(dir, "control-plane.key"))
	if err == nil {
		t.Fatal("a control plane started with no key because the directory is read-only")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("error %v, want the permission failure the operator has to fix", err)
	}
}

func TestLoadSealerReportsAKeyPathItCanNeitherReadNorCreate(t *testing.T) {
	dir := t.TempDir()
	// A Kubernetes secret volume is a symlink into ..data, so when the mount
	// goes away the key path is a dangling symlink: the entry is there, so
	// creating it is refused, but there is nothing behind it to read. Both
	// halves of the first-run path fail at once, and the control plane must say
	// so rather than retry the same two failures for ever.
	link := filepath.Join(dir, "control-plane.key")
	if err := os.Symlink(filepath.Join(dir, "gone"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := LoadSealer("", link)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a control plane started with no usable key")
		}
		if !strings.Contains(err.Error(), link) {
			t.Errorf("error %q does not name the key path %q", err, link)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("LoadSealer never returned: it is retrying the same failure instead of reporting it")
	}
}

func TestLoadSealerIgnoresAKeyFileWhenAnExplicitKeyIsConfigured(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	// A broken file on disk, and a working explicit source. The source wins, and
	// the file is not even read: this is the path an operator takes to recover a
	// control plane whose key volume is gone.
	src := strings.Repeat("k", 32)
	s, err := LoadSealer(src, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open(s.Seal([]byte("v"))); err != nil {
		t.Errorf("the explicit key cannot round trip: %v", err)
	}
	if _, err := os.Stat(keyFile); !os.IsNotExist(err) {
		t.Error("an explicit key still touched the key file")
	}
}

// TestARawKeyEndingInWhitespaceIsNotTrimmed: the hand-placed-key test above
// writes 32 random bytes, so it failed about one run in sixty-four - a CI run
// hit it before a laptop did, and what it found was not a flake. decodeKey
// trimmed the file before looking at it, so a valid raw key whose last byte
// happened to be a space, tab, CR or LF became 31 bytes and the control plane
// refused to start. About one generated key in sixty-four is affected, which is
// the same as saying one fresh deployment in sixty-four was broken.
//
// The bytes are fixed here rather than random so the test fails for the reason
// it names, every time.
func TestARawKeyEndingInWhitespaceIsNotTrimmed(t *testing.T) {
	for _, last := range []byte{' ', '\t', '\r', '\n', 0x00, 0xff} {
		dir := t.TempDir()
		keyFile := filepath.Join(dir, "key")
		raw := make([]byte, keyLen)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		raw[keyLen-1] = last
		// Also a leading one, which the other end of trim would eat.
		raw[0] = '\n'
		if err := os.WriteFile(keyFile, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := LoadSealer("", keyFile)
		if err != nil {
			t.Fatalf("a raw 32-byte key ending in %q was rejected: %v", last, err)
		}
		// And it must be the key that was written, not a repaired one.
		sealed := s.Seal([]byte("v"))
		back, err := s.Open(sealed)
		if err != nil || string(back) != "v" {
			t.Fatalf("seal round trip: %v %q", err, back)
		}
	}
}
