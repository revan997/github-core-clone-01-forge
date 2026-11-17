// Tests for FindZlibEnd's bulk-read (bufio) implementation.
// Correctness: exact end offsets and inflated bytes on valid streams
// (sizes 0..100 KiB, prefix offsets, trailing pack data), errors on
// corrupt/truncated streams. The single-byte-reader predecessor was A/B'd
// against this implementation before the swap (median 40.6µs vs 12.3µs per
// 1 KiB inflate, identical ends/data); see builder-c REPORT.md.
package store

import (
	"bytes"
	"compress/zlib"
	"strings"
	"testing"
)

func deflateAB(t testing.TB, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFindZlibEndBulkReader(t *testing.T) {
	sizes := []int{0, 1, 17, 100, 1024, 4095, 4096, 4097, 8192, 65536, 100 * 1024}
	for _, size := range sizes {
		payload := []byte(strings.Repeat("abcdefghij0123456789", size/20+1)[:size])
		stream := deflateAB(t, payload)
		for _, prefixLen := range []int{0, 13, 5000} {
			prefix := bytes.Repeat([]byte{0xAB}, prefixLen)
			trailing := []byte("TRAILING-PACK-DATA-1234567890")
			buf := append(append(append([]byte{}, prefix...), stream...), trailing...)
			end, data, err := FindZlibEnd(buf, prefixLen)
			if err != nil {
				t.Fatalf("size %d prefix %d: unexpected error %v", size, prefixLen, err)
			}
			if end != prefixLen+len(stream) {
				t.Fatalf("size %d prefix %d: end %d != start+stream %d", size, prefixLen, end, prefixLen+len(stream))
			}
			if !bytes.Equal(data, payload) {
				t.Fatalf("size %d prefix %d: inflated data mismatch", size, prefixLen)
			}
		}
	}
	payload := []byte(strings.Repeat("x", 1024))
	stream := deflateAB(t, payload)
	corrupt := append([]byte{}, stream...)
	corrupt[len(corrupt)/2] ^= 0xFF
	for name, buf := range map[string][]byte{
		"truncated": stream[:len(stream)-5],
		"corrupt":   corrupt,
		"empty":     {},
		"garbage":   []byte("not a zlib stream at all........"),
	} {
		if _, _, err := FindZlibEnd(buf, 0); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
	}
	if _, _, err := FindZlibEnd(stream, -1); err == nil {
		t.Fatal("negative start must error")
	}
	if _, _, err := FindZlibEnd(stream, len(stream)+1); err == nil {
		t.Fatal("start past end must error")
	}
}

func BenchmarkFindZlibEnd(b *testing.B) {
	payload := []byte(strings.Repeat("bench push change x ", 64)[:1024])
	stream := deflateAB(b, payload)
	pack := append(append([]byte{}, stream...), []byte("TRAILING")...)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		end, data, err := FindZlibEnd(pack, 0)
		if err != nil || len(data) != 1024 || end != len(stream) {
			b.Fatalf("end=%d err=%v", end, err)
		}
	}
}
