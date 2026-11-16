// Package blob implements content addressing, chunking and the envelope that
// every stored blob and manifest is wrapped in (docs/sync-engine.md, 3).
package blob

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// Prefix of every hash string.
const Prefix = "sha256:"

// Hash returns "sha256:<hex>" of data.
func Hash(data []byte) string {
	s := sha256.Sum256(data)
	return Prefix + hex.EncodeToString(s[:])
}

// Valid reports whether h looks like a hash produced by Hash.
func Valid(h string) bool {
	if !strings.HasPrefix(h, Prefix) || len(h) != len(Prefix)+64 {
		return false
	}
	_, err := hex.DecodeString(h[len(Prefix):])
	return err == nil
}

// Path is where a chunk with hash h lives in the store: blobs/<hh>/<hex>.
func Path(h string) string {
	x := strings.TrimPrefix(h, Prefix)
	return "blobs/" + x[:2] + "/" + x
}

// Envelope header (docs/sync-engine.md, 3-3).
var magic = []byte("KRVN")

const (
	envVersion = 1
	codecNone  = 0
	codecZstd  = 1
	cipherNone = 0
	headerLen  = 8
)

var (
	encoder, _ = zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	decoder, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(1<<30))
)

// ErrCorrupt means a stored object could not be decoded or failed its check.
var ErrCorrupt = errors.New("corrupt or unreadable object")

// Seal wraps content in the envelope, compressed with zstd when that helps.
func Seal(content []byte) []byte {
	z := encoder.EncodeAll(content, make([]byte, 0, len(content)/3+64))
	codec := byte(codecZstd)
	payload := z
	if len(z) >= len(content) {
		codec, payload = codecNone, content
	}
	out := make([]byte, 0, headerLen+len(payload))
	out = append(out, magic...)
	out = append(out, envVersion, codec, cipherNone, 0)
	return append(out, payload...)
}

// Open unwraps an envelope.
func Open(sealed []byte) ([]byte, error) {
	if len(sealed) < headerLen || !bytes.Equal(sealed[:4], magic) {
		return nil, fmt.Errorf("%w: bad header", ErrCorrupt)
	}
	if sealed[4] != envVersion {
		return nil, fmt.Errorf("%w: envelope version %d is newer than this Karvan", ErrCorrupt, sealed[4])
	}
	if sealed[6] != cipherNone {
		return nil, fmt.Errorf("%w: encrypted object (cipher %d) needs a newer Karvan", ErrCorrupt, sealed[6])
	}
	payload := sealed[headerLen:]
	switch sealed[5] {
	case codecNone:
		return append([]byte(nil), payload...), nil
	case codecZstd:
		out, err := decoder.DecodeAll(payload, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%w: unknown codec %d", ErrCorrupt, sealed[5])
}

// OpenChunk unwraps a chunk and checks it against its hash.
func OpenChunk(h string, sealed []byte) ([]byte, error) {
	c, err := Open(sealed)
	if err != nil {
		return nil, err
	}
	if Hash(c) != h {
		return nil, fmt.Errorf("%w: hash mismatch for %s", ErrCorrupt, h)
	}
	return c, nil
}

// Chunking parameters (docs/sync-engine.md, 3-2).
const (
	SingleMax   = 512 << 10 // files up to this size are one chunk
	lineMin     = 256 << 10 // a chunk may end after a line once it is this long...
	lineForce   = 2 << 20   // ...and must end at the next newline after this...
	hardMax     = 4 << 20   // ...or here if there is no newline at all
	binaryChunk = 1 << 20
)

// Chunk splits content. Boundaries depend only on content, and for text they
// fall after a newline, so appending lines changes only the last chunk.
func Chunk(content []byte) [][]byte {
	if len(content) <= SingleMax {
		return [][]byte{content}
	}
	if bytes.IndexByte(content, '\n') < 0 {
		var out [][]byte
		for len(content) > 0 {
			n := min(binaryChunk, len(content))
			out = append(out, content[:n])
			content = content[n:]
		}
		return out
	}
	var out [][]byte
	start := 0
	pos := 0
	for pos < len(content) {
		nl := bytes.IndexByte(content[pos:], '\n')
		end := len(content)
		if nl >= 0 {
			end = pos + nl + 1
		}
		if end-start > hardMax && nl < 0 || end-start > hardMax+lineForce {
			// No newline for too long: cut hard.
			cut := start + hardMax
			out = append(out, content[start:cut])
			start, pos = cut, cut
			continue
		}
		line := content[pos:end]
		pos = end
		size := pos - start
		if nl < 0 {
			break
		}
		h := sha256.Sum256(line)
		if size >= lineForce || size >= lineMin && h[0]&0x07 == 0 {
			out = append(out, content[start:pos])
			start = pos
		}
	}
	if start < len(content) {
		out = append(out, content[start:])
	}
	return out
}

// Split returns the chunk hashes of content and a map hash -> chunk.
func Split(content []byte) ([]string, map[string][]byte) {
	parts := Chunk(content)
	hs := make([]string, len(parts))
	m := make(map[string][]byte, len(parts))
	for i, p := range parts {
		hs[i] = Hash(p)
		m[hs[i]] = p
	}
	return hs, m
}
