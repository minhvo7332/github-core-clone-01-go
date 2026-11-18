// Shared helpers: SHA-1 object ids, base64url global ids, git pkt-lines,
// pack varints, pooled zlib. Stdlib only; nothing here touches the network
// or the disk.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"io"
	"sync"
)

const zeroOID = "0000000000000000000000000000000000000000"
const agentName = "github-core-clone/1.0.0"
const viewerLogin = "adaiversen"
const viewerID = 1000

func sha1Hex(data []byte) string {
	sum := sha1.Sum(data)
	return hex.EncodeToString(sum[:])
}

// objectID returns the git id of a "type <len>\0<content>" object.
func objectID(typ string, content []byte) string {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte(" "))
	h.Write([]byte(itoa(len(content))))
	h.Write([]byte{0})
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func b64urlEncode(text string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(text))
}

func b64urlDecode(text string) (string, bool) {
	b, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil {
		return "", false
	}
	return string(b), true
}

func isSHA1(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// pktLine frames one pkt-line: 4 hex digits of (payload length + 4).
func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	head := []byte("0000")
	hexDigits := "0123456789abcdef"
	head[0] = hexDigits[(n>>12)&0xf]
	head[1] = hexDigits[(n>>8)&0xf]
	head[2] = hexDigits[(n>>4)&0xf]
	head[3] = hexDigits[n&0xf]
	out := make([]byte, 0, n)
	out = append(out, head...)
	out = append(out, payload...)
	return out
}

func pktLineString(s string) []byte { return pktLine([]byte(s)) }

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// pktPart is one parsed pkt-line: nil = flush, "delim", or a payload.
type pktPart struct {
	flush bool
	delim bool
	data  []byte
}

func readPktLines(buf []byte) []pktPart {
	var out []pktPart
	pos := 0
	for pos+4 <= len(buf) {
		n := 0
		for i := 0; i < 4; i++ {
			c := buf[pos+i]
			var v int
			switch {
			case c >= '0' && c <= '9':
				v = int(c - '0')
			case c >= 'a' && c <= 'f':
				v = int(c-'a') + 10
			case c >= 'A' && c <= 'F':
				v = int(c-'A') + 10
			default:
				return out
			}
			n = n*16 + v
		}
		if n == 0 {
			out = append(out, pktPart{flush: true})
			pos += 4
			continue
		}
		if n == 1 {
			out = append(out, pktPart{delim: true})
			pos += 4
			continue
		}
		if pos+n > len(buf) {
			break
		}
		out = append(out, pktPart{data: buf[pos+4 : pos+n]})
		pos += n
	}
	return out
}

// pktTextLines returns the payloads of a pkt-line stream as strings.
func pktTextLines(buf []byte) []string {
	var lines []string
	for _, p := range readPktLines(buf) {
		if p.data != nil {
			lines = append(lines, string(p.data))
		}
	}
	return lines
}

// encodeVarint is git's pack entry varint: type (3 bits) + size.
func encodeVarint(typeNum, size int) []byte {
	var out []byte
	first := byte(typeNum<<4) | byte(size&0x0f)
	size >>= 4
	if size != 0 {
		first |= 0x80
	}
	out = append(out, first)
	for size != 0 {
		b := byte(size & 0x7f)
		size >>= 7
		if size != 0 {
			b |= 0x80
		}
		out = append(out, b)
	}
	return out
}

// --- pooled zlib ---

var deflateBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func zlibDeflate(data []byte) []byte {
	buf := deflateBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	w, _ := zlib.NewWriterLevel(buf, 6)
	w.Write(data)
	w.Close()
	out := make([]byte, buf.Len())
	copy(out, buf.Bytes())
	deflateBufPool.Put(buf)
	return out
}

// countReader counts exactly the bytes flate pulls from it. It implements
// io.ByteReader so flate reads byte-by-byte with no read-ahead buffering,
// which makes Consumed exact.
type countReader struct {
	b   []byte
	pos int
}

func (r *countReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

func (r *countReader) ReadByte() (byte, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	c := r.b[r.pos]
	r.pos++
	return c, nil
}

// zlibInflateWithLength inflates one zlib stream and reports the input bytes
// consumed, so a pack walker can find the next entry.
func zlibInflateWithLength(buf []byte) (data []byte, consumed int, err error) {
	cr := &countReader{b: buf}
	zr, err := zlib.NewReader(cr)
	if err != nil {
		return nil, 0, err
	}
	defer zr.Close()
	data, err = io.ReadAll(zr)
	if err != nil {
		return nil, 0, err
	}
	return data, cr.pos, nil
}

func zlibInflate(data []byte) ([]byte, error) {
	out, _, err := zlibInflateWithLength(data)
	return out, err
}
