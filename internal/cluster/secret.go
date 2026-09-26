package cluster

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Sealer encrypts cluster credentials at rest.
//
// The existing API keys deliberately store only a hash (internal/auth/auth.go:
// CreateKey), which is right for a key a client presents. A cluster credential
// is different: the control plane must be able to show it to an administrator
// again and must be able to log into the cluster it created, so the plaintext
// has to survive. That is why this exists, and why its limitation is written
// down rather than implied: anyone who can read the key file can decrypt every
// cluster credential. The mitigations are the adminOnly gate on the reveal route
// and an audit row, not defence in depth.
type Sealer struct{ aead cipher.AEAD }

// NewSealer builds a sealer from a 32-byte key.
func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("control-plane key must be 32 bytes, got %d", len(key))
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// ErrNoKey means no control-plane key was configured and none could be found.
var ErrNoKey = errors.New("no control-plane key")

// keyLen is the one length a control-plane key is, whichever form it arrives
// in: raw bytes, or hex that decodes to them.
const keyLen = 32

// LoadSealer resolves the key from source, falling back to the key file, and
// generates the file on first run. The value is never logged, only the path.
func LoadSealer(source, keyFile string) (*Sealer, error) {
	if source != "" {
		return NewSealer([]byte(source))
	}
	if b, err := os.ReadFile(keyFile); err == nil {
		s, err := NewSealer(decodeKey(b))
		if err != nil {
			return nil, fmt.Errorf("control-plane key in %s: %w", keyFile, err)
		}
		return s, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(keyFile), 0o700); err != nil {
		return nil, err
	}
	// Create with O_EXCL so two control planes racing on a fresh volume cannot
	// each believe they own the key and each write a different one.
	f, err := os.OpenFile(keyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		// Lost the race: the winner's key is the real one, so read it rather
		// than write over it. This re-reads once and does not recurse. A
		// dangling symlink at the key path — which is what a Kubernetes secret
		// volume leaves behind when the mount goes away — is both unreadable
		// and uncreatable, so recursing here would spin for ever instead of
		// starting.
		b, rerr := os.ReadFile(keyFile)
		if rerr != nil {
			return nil, fmt.Errorf("control-plane key %s appeared but cannot be read: %w", keyFile, rerr)
		}
		s, err := NewSealer(decodeKey(b))
		if err != nil {
			return nil, fmt.Errorf("control-plane key in %s: %w", keyFile, err)
		}
		return s, nil
	}
	defer f.Close()
	if _, err := fmt.Fprintf(f, "%x\n", key); err != nil {
		return nil, err
	}
	return NewSealer(key)
}

func trim(b []byte) []byte {
	i := 0
	for i < len(b) && (b[i] == '\n' || b[i] == '\r' || b[i] == ' ' || b[i] == '\t') {
		i++
	}
	j := len(b)
	for j > i && (b[j-1] == '\n' || b[j-1] == '\r' || b[j-1] == ' ' || b[j-1] == '\t') {
		j--
	}
	return b[i:j]
}

// decodeKey reads the key file, which holds hex, and tolerates raw 32 bytes so
// a hand-placed key still works. The hex is what the file is written as because
// a binary key in a text file is unreviewable and gets mangled by every copy.
func decodeKey(b []byte) []byte {
	// A raw binary key is used byte for byte, and trimming it is wrong: a
	// generated key ends in a space, tab, CR or LF about one time in sixty-four,
	// and trimming turns a valid 32-byte key into 31 and refuses to start. The
	// length is the whole test - a file already the right size is not text,
	if len(b) == keyLen {
		return b
	}
	t := trim(b)
	if len(t) == keyLen {
		return t
	}
	raw, err := hex.DecodeString(string(t))
	if err != nil {
		return t // let NewSealer report the real length problem
	}
	return raw
}

// Seal returns the nonce followed by the ciphertext. The nonce is not a secret,
// so it is stored in the clear with the ciphertext rather than in a column.
func (s *Sealer) Seal(plain []byte) []byte {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		// crypto/rand failing is not recoverable and must not be swallowed.
		panic("cluster: crypto/rand unavailable: " + err.Error())
	}
	return s.aead.Seal(nonce, nonce, plain, nil)
}

// Open reverses Seal. A wrong key or tampered ciphertext is an error, never a
// partial read.
func (s *Sealer) Open(sealed []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return nil, fmt.Errorf("sealed value too short: %d bytes", len(sealed))
	}
	out, err := s.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return nil, fmt.Errorf("cannot decrypt: %w", err)
	}
	return out, nil
}
