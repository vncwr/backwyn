package crypto_test

import (
	"bytes"
	"crypto/rand"
	"io"
	"strings"
	"testing"

	"github.com/vncwr/backwyn/internal/crypto"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return k
}

func encrypt(t *testing.T, plaintext []byte, key []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := crypto.Encrypt(&buf, bytes.NewReader(plaintext), key); err != nil {
		t.Fatalf("crypto.Encrypt: %v", err)
	}
	return buf.Bytes()
}

func decrypt(t *testing.T, ciphertext []byte, key []byte) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	err := crypto.Decrypt(&out, bytes.NewReader(ciphertext), key)
	return out.Bytes(), err
}

// TestRoundTrip covers the frame boundaries, which is where a chunked format is
// most likely to be wrong: empty input, and sizes just under/at/over crypto.ChunkSize.
func TestRoundTrip(t *testing.T) {
	key := testKey(t)
	sizes := []int{
		0,
		1,
		crypto.ChunkSize - 1,
		crypto.ChunkSize,
		crypto.ChunkSize + 1,
		3*crypto.ChunkSize + 17,
	}
	for _, size := range sizes {
		plaintext := make([]byte, size)
		if _, err := rand.Read(plaintext); err != nil {
			t.Fatalf("generate plaintext: %v", err)
		}

		got, err := decrypt(t, encrypt(t, plaintext, key), key)
		if err != nil {
			t.Fatalf("size %d: crypto.Decrypt: %v", size, err)
		}
		if !bytes.Equal(got, plaintext) {
			t.Errorf("size %d: round trip mismatch (got %d bytes)", size, len(got))
		}
	}
}

func TestCiphertextIsNotPlaintext(t *testing.T) {
	key := testKey(t)
	plaintext := bytes.Repeat([]byte("SECRET-CUSTOMER-DATA"), 100)
	ct := encrypt(t, plaintext, key)

	if bytes.Contains(ct, []byte("SECRET-CUSTOMER-DATA")) {
		t.Fatal("plaintext appears verbatim in ciphertext")
	}
	if !bytes.HasPrefix(ct, []byte(crypto.MagicBytes)) {
		t.Errorf("ciphertext should start with magic %q", crypto.MagicBytes)
	}
}

// TestNoncesAreUniquePerFrame guards the one property AES-GCM cannot survive
// losing: a repeated nonce under the same key breaks confidentiality.
func TestNoncesAreUniquePerFrame(t *testing.T) {
	key := testKey(t)
	// Force several frames.
	ct := encrypt(t, make([]byte, 5*crypto.ChunkSize), key)

	seen := map[string]bool{}
	r := bytes.NewReader(ct[len(crypto.MagicBytes):])
	for {
		var lenHdr [4]byte
		if _, err := io.ReadFull(r, lenHdr[:]); err != nil {
			t.Fatalf("read frame length: %v", err)
		}
		frameLen := uint32(lenHdr[0])<<24 | uint32(lenHdr[1])<<16 | uint32(lenHdr[2])<<8 | uint32(lenHdr[3])
		if frameLen == 0 {
			break
		}
		nonce := make([]byte, crypto.NonceSize)
		if _, err := io.ReadFull(r, nonce); err != nil {
			t.Fatalf("read nonce: %v", err)
		}
		if seen[string(nonce)] {
			t.Fatalf("nonce reused across frames: %x", nonce)
		}
		seen[string(nonce)] = true
		if _, err := io.CopyN(io.Discard, r, int64(frameLen)); err != nil {
			t.Fatalf("skip ciphertext: %v", err)
		}
	}
	if len(seen) < 5 {
		t.Fatalf("expected at least 5 frames, saw %d", len(seen))
	}
}

func TestWrongKeyFails(t *testing.T) {
	plaintext := []byte("the quick brown fox")
	ct := encrypt(t, plaintext, testKey(t))

	got, err := decrypt(t, ct, testKey(t))
	if err == nil {
		t.Fatal("crypto.Decrypt with the wrong key must fail, got nil error")
	}
	if bytes.Contains(got, plaintext) {
		t.Error("plaintext leaked despite decryption failure")
	}
}

// TestBitFlipDetected is the property the whole product rests on: a corrupted
// artifact must fail loudly rather than yield subtly wrong data.
func TestBitFlipDetected(t *testing.T) {
	key := testKey(t)
	ct := encrypt(t, bytes.Repeat([]byte("data"), 5000), key)

	// Flip a bit in each region of the stream: nonce, ciphertext body, and tail.
	for _, pos := range []int{len(crypto.MagicBytes) + 5, len(ct) / 2, len(ct) - 8} {
		tampered := make([]byte, len(ct))
		copy(tampered, ct)
		tampered[pos] ^= 0xFF

		if _, err := decrypt(t, tampered, key); err == nil {
			t.Errorf("bit flip at offset %d was not detected", pos)
		}
	}
}

// TestTruncationDetected covers the failure the framing format exists to catch.
// The invariant is that crypto.Decrypt *reports* truncation — never that it declines
// to write. The terminator-dropped case is the instructive one: every data
// frame is intact, so dst legitimately receives the full plaintext, and only
// the missing terminator reveals the stream was cut. Callers must therefore
// gate on the error, not on how much output they got.
func TestTruncationDetected(t *testing.T) {
	key := testKey(t)
	plaintext := make([]byte, 3*crypto.ChunkSize)
	if _, err := rand.Read(plaintext); err != nil {
		t.Fatalf("generate plaintext: %v", err)
	}
	ct := encrypt(t, plaintext, key)

	cuts := map[string]int{
		"mid-frame":          len(ct) / 2,
		"terminator dropped": len(ct) - 4,
		"header only":        len(crypto.MagicBytes),
		"truncated magic":    2,
		"empty":              0,
	}
	for name, cut := range cuts {
		if _, err := decrypt(t, ct[:cut], key); err == nil {
			t.Errorf("%s: truncated stream decoded without error", name)
		}
	}

	// A cut that loses data must additionally not yield the full plaintext.
	if got, _ := decrypt(t, ct[:len(ct)/2], key); bytes.Equal(got, plaintext) {
		t.Error("mid-frame truncation produced full plaintext")
	}
}

// TestDstIsUntrustworthyOnError pins the contract the truncation behaviour
// implies and that internal/artifact depends on: when crypto.Decrypt fails, whatever
// reached dst must be discarded, because it may be complete-looking but is not
// attested. Backed by the terminator-dropped case, where dst holds every byte.
func TestDstIsUntrustworthyOnError(t *testing.T) {
	key := testKey(t)
	plaintext := bytes.Repeat([]byte("row"), 4000)
	ct := encrypt(t, plaintext, key)

	var out bytes.Buffer
	err := crypto.Decrypt(&out, bytes.NewReader(ct[:len(ct)-4]), key)
	if err == nil {
		t.Fatal("dropping the terminator must be reported as an error")
	}
	if out.Len() != len(plaintext) {
		t.Fatalf("expected dst to hold the full %d bytes despite the error, got %d",
			len(plaintext), out.Len())
	}
}

func TestBadMagicRejected(t *testing.T) {
	key := testKey(t)
	ct := encrypt(t, []byte("hello"), key)
	ct[0] = 'X'

	if _, err := decrypt(t, ct, key); err == nil {
		t.Fatal("bad magic must be rejected")
	} else if !strings.Contains(err.Error(), "magic") {
		t.Errorf("error should mention the magic header, got: %v", err)
	}
}

// TestPgDumpArchiveIsNotMistakenForArtifact: a plain pg_dump custom archive
// starts with "PGDMP", not our magic. Feeding one to crypto.Decrypt must fail cleanly
// rather than emit garbage.
func TestPgDumpArchiveIsNotMistakenForArtifact(t *testing.T) {
	if _, err := decrypt(t, []byte("PGDMP\x01\x0e\x00"), testKey(t)); err == nil {
		t.Fatal("a raw pg_dump archive must not decrypt as a backwyn artifact")
	}
}

func TestKeyLengthValidated(t *testing.T) {
	for _, n := range []int{0, 16, 24, 31, 33} {
		key := make([]byte, n)
		if err := crypto.Encrypt(io.Discard, strings.NewReader("x"), key); err == nil {
			t.Errorf("crypto.Encrypt accepted a %d-byte key", n)
		}
		if err := crypto.Decrypt(io.Discard, strings.NewReader("x"), key); err == nil {
			t.Errorf("crypto.Decrypt accepted a %d-byte key", n)
		}
	}
}

// errReader fails partway through, standing in for a storage backend that dies
// mid-download.
type errReader struct {
	data []byte
	n    int
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.n >= len(e.data) {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, e.data[e.n:])
	e.n += n
	return n, nil
}

func TestReadErrorPropagates(t *testing.T) {
	key := testKey(t)
	ct := encrypt(t, bytes.Repeat([]byte("x"), 2*crypto.ChunkSize), key)

	var out bytes.Buffer
	if err := crypto.Decrypt(&out, &errReader{data: ct[:len(ct)/2]}, key); err == nil {
		t.Fatal("a mid-stream read error must surface, not be treated as EOF")
	}
}
