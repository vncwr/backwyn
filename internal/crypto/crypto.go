// package crypto provides streaming aes-256-gcm encryption.
// format: 4-byte magic, then frames of [uint32 len][12-byte nonce][ciphertext].
// a zero-length frame terminates the stream.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
)

// exported for testing.
const (
	ChunkSize = 64 * 1024
	NonceSize = 12

	// magic bytes identifying a backwyn artifact.
	MagicBytes = "BWY1"
)

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// encrypt reads plaintext and writes framed ciphertext.
func Encrypt(dst io.Writer, src io.Reader, key []byte) error {
	aead, err := newAEAD(key)
	if err != nil {
		return err
	}
	if _, err := dst.Write([]byte(MagicBytes)); err != nil {
		return fmt.Errorf("write magic: %w", err)
	}

	buf := make([]byte, ChunkSize)
	for {
		n, readErr := io.ReadFull(src, buf)
		if n > 0 {
			nonce := make([]byte, NonceSize)
			if _, err := rand.Read(nonce); err != nil {
				return fmt.Errorf("generate nonce: %w", err)
			}
			ct := aead.Seal(nil, nonce, buf[:n], nil)

			var lenHdr [4]byte
			binary.BigEndian.PutUint32(lenHdr[:], uint32(len(ct)))
			if _, err := dst.Write(lenHdr[:]); err != nil {
				return fmt.Errorf("write frame length: %w", err)
			}
			if _, err := dst.Write(nonce); err != nil {
				return fmt.Errorf("write nonce: %w", err)
			}
			if _, err := dst.Write(ct); err != nil {
				return fmt.Errorf("write ciphertext: %w", err)
			}
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read plaintext: %w", readErr)
		}
	}

	// terminator frame.
	var term [4]byte
	if _, err := dst.Write(term[:]); err != nil {
		return fmt.Errorf("write terminator: %w", err)
	}
	return nil
}

// decrypt reads framed ciphertext and writes plaintext.
func Decrypt(dst io.Writer, src io.Reader, key []byte) error {
	aead, err := newAEAD(key)
	if err != nil {
		return err
	}

	magic := make([]byte, len(MagicBytes))
	if _, err := io.ReadFull(src, magic); err != nil {
		return fmt.Errorf("read magic: %w", err)
	}
	if string(magic) != MagicBytes {
		return fmt.Errorf("bad magic header: not a backwyn artifact")
	}

	for {
		var lenHdr [4]byte
		if _, err := io.ReadFull(src, lenHdr[:]); err != nil {
			return fmt.Errorf("read frame length: %w", err)
		}
		frameLen := binary.BigEndian.Uint32(lenHdr[:])
		if frameLen == 0 {
			return nil // terminator frame
		}

		nonce := make([]byte, NonceSize)
		if _, err := io.ReadFull(src, nonce); err != nil {
			return fmt.Errorf("read nonce: %w", err)
		}
		ct := make([]byte, frameLen)
		if _, err := io.ReadFull(src, ct); err != nil {
			return fmt.Errorf("read ciphertext: %w", err)
		}
		pt, err := aead.Open(nil, nonce, ct, nil)
		if err != nil {
			return fmt.Errorf("decrypt frame (corrupt or wrong key): %w", err)
		}
		if _, err := dst.Write(pt); err != nil {
			return fmt.Errorf("write plaintext: %w", err)
		}
	}
}
