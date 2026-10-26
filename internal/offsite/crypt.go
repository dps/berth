// Package offsite copies Berth's backups to an S3-compatible bucket
// (Cloudflare R2, Backblaze B2, AWS S3, MinIO), encrypted on the Mac first
// so the bucket only ever holds ciphertext (design.md, Off-Mac backups).
package offsite

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// The file format: magic, a random 16-byte nonce prefix, then chunks. Each
// chunk is up to chunkSize bytes of plaintext sealed with XChaCha20-
// Poly1305 under nonce = prefix || chunk counter, with associated data
// saying whether it is the last chunk. Reordering, truncation and
// tampering all fail to decrypt.
const (
	magic     = "BERTHENC1\n"
	chunkSize = 64 << 10
)

// KeySize is the length of an encryption key.
const KeySize = chacha20poly1305.KeySize

// NewKey returns a fresh random key.
func NewKey() []byte {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return k
}

// EncodeKey and ParseKey write keys the way people save them.
func EncodeKey(k []byte) string { return "berthkey_" + base64.RawURLEncoding.EncodeToString(k) }

func ParseKey(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(strings.TrimSpace(s), "berthkey_"))
	if err != nil || len(b) != KeySize {
		return nil, errors.New("that isn't a Berth backup key (berthkey_…)")
	}
	return b, nil
}

func chunkNonce(prefix []byte, n uint64) []byte {
	nonce := make([]byte, chacha20poly1305.NonceSizeX)
	copy(nonce, prefix)
	binary.BigEndian.PutUint64(nonce[16:], n)
	return nonce
}

// Encrypt copies r to w encrypted with key.
func Encrypt(w io.Writer, r io.Reader, key []byte) error {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return err
	}
	prefix := make([]byte, 16)
	if _, err := rand.Read(prefix); err != nil {
		return err
	}
	if _, err := io.WriteString(w, magic); err != nil {
		return err
	}
	if _, err := w.Write(prefix); err != nil {
		return err
	}
	// Read one chunk ahead so the last one can be marked.
	buf, next := make([]byte, chunkSize), make([]byte, chunkSize)
	n, err := io.ReadFull(r, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return err
	}
	var out []byte
	for i := uint64(0); ; i++ {
		m, nerr := 0, error(nil)
		if n == chunkSize {
			m, nerr = io.ReadFull(r, next)
			if nerr != nil && nerr != io.EOF && nerr != io.ErrUnexpectedEOF {
				return nerr
			}
		}
		last := m == 0
		ad := []byte{0}
		if last {
			ad[0] = 1
		}
		out = aead.Seal(out[:0], chunkNonce(prefix, i), buf[:n], ad)
		if _, err := w.Write(out); err != nil {
			return err
		}
		if last {
			return nil
		}
		buf, next, n = next, buf, m
	}
}

// Decrypt copies an encrypted stream from r to w.
func Decrypt(w io.Writer, r io.Reader, key []byte) error {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return err
	}
	head := make([]byte, len(magic)+16)
	if _, err := io.ReadFull(r, head); err != nil || !bytes.HasPrefix(head, []byte(magic)) {
		return errors.New("not a Berth encrypted backup")
	}
	prefix := head[len(magic):]
	sealed := chunkSize + aead.Overhead()
	buf, next := make([]byte, sealed), make([]byte, sealed)
	n, err := io.ReadFull(r, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return err
	}
	var out []byte
	for i := uint64(0); ; i++ {
		m := 0
		if n == sealed {
			m, err = io.ReadFull(r, next)
			if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
				return err
			}
		}
		ad := []byte{0}
		if m == 0 {
			ad[0] = 1
		}
		out, err = aead.Open(out[:0], chunkNonce(prefix, i), buf[:n], ad)
		if err != nil {
			return fmt.Errorf("the backup is damaged, cut short, or was encrypted with another key")
		}
		if _, err := w.Write(out); err != nil {
			return err
		}
		if m == 0 {
			return nil
		}
		buf, next, n = next, buf, m
	}
}
