package offsite

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func TestEncryptRoundTrip(t *testing.T) {
	key := NewKey()
	for _, size := range []int{0, 1, chunkSize - 1, chunkSize, chunkSize + 1, 3*chunkSize + 17} {
		plain := make([]byte, size)
		rand.Read(plain)
		var enc, dec bytes.Buffer
		if err := Encrypt(&enc, bytes.NewReader(plain), key); err != nil {
			t.Fatal(err)
		}
		if err := Decrypt(&dec, bytes.NewReader(enc.Bytes()), key); err != nil || !bytes.Equal(dec.Bytes(), plain) {
			t.Fatalf("size %d: %v", size, err)
		}
		if size == 3*chunkSize+17 {
			b := enc.Bytes()
			// Cut at a chunk boundary: the missing last-chunk mark gives it away.
			cut := len(magic) + 16 + 2*(chunkSize+16)
			if err := Decrypt(&bytes.Buffer{}, bytes.NewReader(b[:cut]), key); err == nil {
				t.Fatal("truncated backup decrypted")
			}
			flipped := append([]byte{}, b...)
			flipped[len(flipped)/2] ^= 1
			if err := Decrypt(&bytes.Buffer{}, bytes.NewReader(flipped), key); err == nil {
				t.Fatal("tampered backup decrypted")
			}
			if err := Decrypt(&bytes.Buffer{}, bytes.NewReader(b), NewKey()); err == nil {
				t.Fatal("decrypted with the wrong key")
			}
		}
	}
	k, err := ParseKey(EncodeKey(key))
	if err != nil || !bytes.Equal(k, key) {
		t.Fatal("key encoding")
	}
	if _, err := ParseKey("berthkey_short"); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestRecoveryCode(t *testing.T) {
	cfg := &Config{Endpoint: "https://acct.r2.cloudflarestorage.com", Bucket: "bkt", Prefix: "berth/mini/", AccessKey: "a", SecretKey: "s", Keep: 14}
	key := NewKey()
	code := RecoveryCode(cfg, key)
	got, k, err := ParseRecoveryCode(code)
	if err != nil || *got != *cfg || !bytes.Equal(k, key) {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if k2, err := ParseAnyKey(code); err != nil || !bytes.Equal(k2, key) {
		t.Fatal("key from a recovery code")
	}
	if k2, err := ParseAnyKey(EncodeKey(key)); err != nil || !bytes.Equal(k2, key) {
		t.Fatal("plain key")
	}
	if _, _, err := ParseRecoveryCode(code[:len(code)-10]); err == nil {
		t.Fatal("cut-off code accepted")
	}
}
