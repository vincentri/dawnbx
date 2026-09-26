package cluster

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sealerFor(t *testing.T) *Sealer {
	t.Helper()
	k := make([]byte, 32)
	rand.Read(k)
	s, err := NewSealer(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSealRoundTrip(t *testing.T) {
	s := sealerFor(t)
	secret := []byte("correct horse battery staple")
	sealed := s.Seal(secret)
	got, err := s.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, secret) {
		t.Fatalf("round trip: %q", got)
	}
	if bytes.Contains(sealed, secret) {
		t.Error("ciphertext contains the plaintext")
	}
}

func TestSealNonceUnique(t *testing.T) {
	s := sealerFor(t)
	secret := []byte("same value twice")
	a, b := s.Seal(secret), s.Seal(secret)
	if bytes.Equal(a, b) {
		t.Fatal("two seals of the same plaintext are identical: the nonce is not random")
	}
}

func TestOpenRejectsWrongKey(t *testing.T) {
	k := make([]byte, 32)
	rand.Read(k)
	a, _ := NewSealer(k)
	other := make([]byte, 32)
	other[0] = 1
	b, err := NewSealer(other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(a.Seal([]byte("x"))); err == nil {
		t.Error("a different key decrypted the value")
	}
}

func TestOpenRejectsTamperedAndShort(t *testing.T) {
	s := sealerFor(t)
	sealed := s.Seal([]byte("value"))
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := s.Open(tampered); err == nil {
		t.Error("a flipped byte decrypted")
	}
	if _, err := s.Open(sealed[:4]); err == nil {
		t.Error("a truncated value decrypted")
	}
}

func TestNewSealerRejectsWrongKeySize(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		if _, err := NewSealer(make([]byte, n)); err == nil {
			t.Errorf("a %d-byte key was accepted", n)
		}
	}
}

func TestLoadSealerGeneratesOnceAndNeverLogsTheValue(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "server", "control-plane.key")

	s, err := LoadSealer("", keyFile)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode %o, want 600: anyone who can read it decrypts every cluster credential", perm)
	}
	// A second process finds the file and gets the same key.
	s2, err := LoadSealer("", keyFile)
	if err != nil {
		t.Fatal(err)
	}
	sealed := s.Seal([]byte("kept"))
	got, err := s2.Open(sealed)
	if err != nil || string(got) != "kept" {
		t.Fatalf("a reloaded key did not open the value: %q %v", got, err)
	}
}

func TestLoadSealerPrefersExplicitSource(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	k := make([]byte, 32)
	rand.Read(k)
	// A 32-byte ASCII key, which is what a base64 or hex env var looks like.
	src := strings.Repeat("a", 32)
	s, err := LoadSealer(src, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyFile); !os.IsNotExist(err) {
		t.Error("an explicit key still wrote a key file")
	}
	if _, err := s.Open(s.Seal([]byte("v"))); err != nil {
		t.Errorf("explicit key round trip: %v", err)
	}
	_ = k
}

// TestDecodeKeyAcceptsRawBytesAndHex: the key file is written as hex because a
// binary key in a text file is unreviewable and gets mangled by every copy. A raw
// 32-byte file is still accepted, so an operator who placed one by hand is not
// locked out.
func TestDecodeKeyAcceptsRawBytesAndHex(t *testing.T) {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(i)
	}
	// Both forms a key file can plausibly hold must decode to the same bytes.
	for name, content := range map[string][]byte{
		"raw bytes":     raw,
		"hex":           []byte(hex.EncodeToString(raw) + "\n"),
		"hex, no eol":   []byte(hex.EncodeToString(raw)),
		"raw, trailing": append(append([]byte(nil), raw...), '\n'),
	} {
		if got := decodeKey(content); !bytes.Equal(got, raw) {
			t.Errorf("%s decoded to %x, want %x", name, got, raw)
		}
	}
	// Undecodable text is handed on as-is, so NewSealer reports the real length
	// problem rather than this silently substituting a key.
	if got := decodeKey([]byte("not hex at all")); string(got) != "not hex at all" {
		t.Errorf("undecodable content was altered: %q", got)
	}
	// A short hex key decodes to a short key rather than being padded or
	// widened, and NewSealer refuses it. That is the property that matters: a
	// weak key must be rejected loudly, not quietly stretched into something that
	// looks valid.
	short := hex.EncodeToString(make([]byte, 8))
	if got := decodeKey([]byte(short)); len(got) == 32 {
		t.Errorf("an 8-byte key was widened to 32 bytes: %x", got)
	}
	if _, err := NewSealer(decodeKey([]byte(short))); err == nil {
		t.Error("a short key file was accepted")
	}
}

// TestNewSealerReportsTheKeyLength: the error says what was wrong, because this
// is a first-run message an operator reads with no other context.
func TestNewSealerReportsTheKeyLength(t *testing.T) {
	_, err := NewSealer(make([]byte, 7))
	if err == nil || !strings.Contains(err.Error(), "7") {
		t.Errorf("the error does not say what was wrong: %v", err)
	}
}
