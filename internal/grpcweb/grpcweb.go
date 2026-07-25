// Package grpcweb implements just enough gRPC-Web framing and protobuf wire
// decoding to read xAI's GetGrokCreditsConfig response.
//
// Ported from ~/.pi/agent/extensions/xai-supergrok-usage.ts, which already had
// this endpoint reverse-engineered and working. There is no .proto available,
// so fields are addressed by number:
//
//	message envelope { CreditsConfig config = 1; }
//	message CreditsConfig {
//	  float usage_percent = 1;   // consumed, 0-100 (NOT remaining)
//	  ...
//	  Timestamp window_end = 5;  // its field 1 varint = epoch seconds
//	}
package grpcweb

import (
	"encoding/binary"
	"errors"
	"math"
	"time"
)

// Wire types we understand.
const (
	wtVarint  = 0
	wtFixed64 = 1
	wtBytes   = 2
	wtFixed32 = 5
)

// EmptyMessage is an empty protobuf message in a gRPC-Web frame: the request
// body for GetGrokCreditsConfig.
var EmptyMessage = []byte{0, 0, 0, 0, 0}

var (
	ErrEmptyMessage  = errors.New("empty gRPC message")
	ErrTruncated     = errors.New("truncated varint")
	ErrVarintTooLong = errors.New("varint too long")
	ErrNoUsage       = errors.New("usage percent not found in response")
)

// Credits is the decoded payload.
type Credits struct {
	UsagePercent float64
	ResetAt      *time.Time
}

// readVarint decodes a base-128 varint at buf[i:].
func readVarint(buf []byte, i int) (uint64, int, error) {
	var x uint64
	var shift uint
	for i < len(buf) {
		b := buf[i]
		i++
		x |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return x, i, nil
		}
		shift += 7
		if shift > 63 {
			return 0, i, ErrVarintTooLong
		}
	}
	return 0, i, ErrTruncated
}

// Unframe extracts the concatenated message payload from gRPC-Web frames.
// Each frame is a 5-byte header (1 flag byte + big-endian uint32 length);
// flag 0 carries the message, 0x80 carries trailers which we ignore.
func Unframe(raw []byte) ([]byte, error) {
	var msg []byte
	for i := 0; i+5 <= len(raw); {
		flag := raw[i]
		n := int(binary.BigEndian.Uint32(raw[i+1 : i+5]))
		i += 5
		if n < 0 || i+n > len(raw) {
			break // truncated frame: use whatever we already have
		}
		data := raw[i : i+n]
		i += n
		if flag == 0 {
			msg = append(msg, data...)
		}
	}
	if len(msg) == 0 {
		return nil, ErrEmptyMessage
	}
	return msg, nil
}

// timestampSeconds reads field 1 of a protobuf Timestamp message.
func timestampSeconds(buf []byte) (int64, bool) {
	var sec int64
	var found bool
	for i := 0; i < len(buf); {
		key, next, err := readVarint(buf, i)
		if err != nil {
			return 0, false
		}
		i = next
		field := key >> 3
		switch key & 7 {
		case wtVarint:
			v, next, err := readVarint(buf, i)
			if err != nil {
				return 0, false
			}
			i = next
			if field == 1 {
				sec, found = int64(v), true
			}
		case wtBytes:
			n, next, err := readVarint(buf, i)
			if err != nil {
				return 0, false
			}
			i = next + int(n)
		case wtFixed32:
			i += 4
		case wtFixed64:
			i += 8
		default:
			return sec, found
		}
		if i > len(buf) {
			return sec, found
		}
	}
	return sec, found
}

// ParseCreditsConfig decodes a raw GetGrokCreditsConfig gRPC-Web response.
func ParseCreditsConfig(raw []byte) (Credits, error) {
	msg, err := Unframe(raw)
	if err != nil {
		return Credits{}, err
	}

	// Outer envelope: field 1, length-delimited, is the CreditsConfig.
	key, i, err := readVarint(msg, 0)
	if err != nil {
		return Credits{}, err
	}
	if key>>3 != 1 || key&7 != wtBytes {
		return Credits{}, errors.New("unexpected outer field in credits response")
	}
	n, i, err := readVarint(msg, i)
	if err != nil {
		return Credits{}, err
	}
	end := i + int(n)
	if end > len(msg) {
		end = len(msg)
	}
	cfg := msg[i:end]

	var out Credits
	var haveUsage bool
	for j := 0; j < len(cfg); {
		key, next, err := readVarint(cfg, j)
		if err != nil {
			return Credits{}, err
		}
		j = next
		field := key >> 3
		switch key & 7 {
		case wtFixed32:
			if j+4 > len(cfg) {
				return Credits{}, ErrTruncated
			}
			v := math.Float32frombits(binary.LittleEndian.Uint32(cfg[j : j+4]))
			j += 4
			if field == 1 {
				out.UsagePercent, haveUsage = float64(v), true
			}
		case wtBytes:
			n, next, err := readVarint(cfg, j)
			if err != nil {
				return Credits{}, err
			}
			j = next
			stop := j + int(n)
			if stop > len(cfg) {
				return Credits{}, ErrTruncated
			}
			chunk := cfg[j:stop]
			j = stop
			if field == 5 {
				if sec, ok := timestampSeconds(chunk); ok && sec > 0 {
					t := time.Unix(sec, 0)
					out.ResetAt = &t
				}
			}
		case wtVarint:
			_, next, err := readVarint(cfg, j)
			if err != nil {
				return Credits{}, err
			}
			j = next
		case wtFixed64:
			j += 8
			if j > len(cfg) {
				return Credits{}, ErrTruncated
			}
		default:
			j = len(cfg) // unknown wire type: stop cleanly
		}
	}

	if !haveUsage || math.IsNaN(out.UsagePercent) || math.IsInf(out.UsagePercent, 0) {
		return Credits{}, ErrNoUsage
	}
	return out, nil
}
