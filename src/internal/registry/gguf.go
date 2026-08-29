// GGUF metadata parsing. Format reference: the header is a magic + version,
// followed by a tensor count and a metadata key/value count, then that many
// key/value pairs, then per-tensor descriptors. We only need the metadata KV
// section for capability detection, but the format is a flat sequential
// stream, so every value — even ones we don't care about — must be read (or
// correctly skipped) in order or the parse desyncs.
package registry

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

const ggufMagic = 0x46554747 // "GGUF" little-endian

type ggufValueType uint32

const (
	gtUint8 ggufValueType = iota
	gtInt8
	gtUint16
	gtInt16
	gtUint32
	gtInt32
	gtFloat32
	gtBool
	gtString
	gtArray
	gtUint64
	gtInt64
	gtFloat64
)

// GGUFMeta holds the subset of metadata Phase 1 routing/UI cares about.
type GGUFMeta struct {
	Architecture   string
	Name           string
	ContextLength  int
	QuantType      string
	SizeLabel      string // e.g. "7B" if the converter recorded one
	IsVisionClip   bool   // architecture == "clip": this file is a multimodal projector
	FileTypeNumber int
}

var errNotGGUF = errors.New("not a GGUF file")

// ParseGGUFMeta reads only the header + metadata KV section (never the tensor
// data), so it stays fast even for multi-gigabyte model files.
func ParseGGUFMeta(path string) (GGUFMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return GGUFMeta{}, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)

	var magic uint32
	if err := binary.Read(r, binary.LittleEndian, &magic); err != nil {
		return GGUFMeta{}, err
	}
	if magic != ggufMagic {
		return GGUFMeta{}, errNotGGUF
	}
	var version uint32
	if err := binary.Read(r, binary.LittleEndian, &version); err != nil {
		return GGUFMeta{}, err
	}
	var tensorCount, kvCount uint64
	if version == 1 {
		var tc32, kv32 uint32
		if err := binary.Read(r, binary.LittleEndian, &tc32); err != nil {
			return GGUFMeta{}, err
		}
		if err := binary.Read(r, binary.LittleEndian, &kv32); err != nil {
			return GGUFMeta{}, err
		}
		tensorCount, kvCount = uint64(tc32), uint64(kv32)
	} else {
		if err := binary.Read(r, binary.LittleEndian, &tensorCount); err != nil {
			return GGUFMeta{}, err
		}
		if err := binary.Read(r, binary.LittleEndian, &kvCount); err != nil {
			return GGUFMeta{}, err
		}
	}
	_ = tensorCount

	meta := GGUFMeta{}
	for i := uint64(0); i < kvCount; i++ {
		key, err := readGGUFString(r, version)
		if err != nil {
			return meta, fmt.Errorf("kv %d key: %w", i, err)
		}
		var vtype uint32
		if err := binary.Read(r, binary.LittleEndian, &vtype); err != nil {
			return meta, fmt.Errorf("kv %d type: %w", i, err)
		}
		val, err := readGGUFValue(r, version, ggufValueType(vtype))
		if err != nil {
			return meta, fmt.Errorf("kv %d value (%s): %w", i, key, err)
		}
		applyGGUFKey(&meta, key, val)
	}
	return meta, nil
}

func applyGGUFKey(meta *GGUFMeta, key string, val any) {
	switch key {
	case "general.architecture":
		if s, ok := val.(string); ok {
			meta.Architecture = s
			meta.IsVisionClip = s == "clip"
		}
	case "general.name":
		if s, ok := val.(string); ok {
			meta.Name = s
		}
	case "general.size_label":
		if s, ok := val.(string); ok {
			meta.SizeLabel = s
		}
	case "general.file_type":
		meta.FileTypeNumber = toInt(val)
	default:
		if hasSuffix(key, ".context_length") && meta.ContextLength == 0 {
			meta.ContextLength = toInt(val)
		}
	}
}

func hasSuffix(s, suf string) bool {
	if len(s) < len(suf) {
		return false
	}
	return s[len(s)-len(suf):] == suf
}

func toInt(v any) int {
	switch n := v.(type) {
	case uint8:
		return int(n)
	case int8:
		return int(n)
	case uint16:
		return int(n)
	case int16:
		return int(n)
	case uint32:
		return int(n)
	case int32:
		return int(n)
	case uint64:
		return int(n)
	case int64:
		return int(n)
	case float32:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func readGGUFString(r io.Reader, version uint32) (string, error) {
	var length uint64
	if version == 1 {
		var l32 uint32
		if err := binary.Read(r, binary.LittleEndian, &l32); err != nil {
			return "", err
		}
		length = uint64(l32)
	} else {
		if err := binary.Read(r, binary.LittleEndian, &length); err != nil {
			return "", err
		}
	}
	if length > 64<<20 { // sanity cap: no legitimate metadata string is >64MB
		return "", fmt.Errorf("implausible string length %d", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

// readGGUFValue reads (or, for types we don't map to Go values below,
// still fully consumes) exactly one value of the given type so the stream
// stays in sync for subsequent keys.
func readGGUFValue(r io.Reader, version uint32, t ggufValueType) (any, error) {
	switch t {
	case gtUint8:
		var v uint8
		return v, binary.Read(r, binary.LittleEndian, &v)
	case gtInt8:
		var v int8
		return v, binary.Read(r, binary.LittleEndian, &v)
	case gtUint16:
		var v uint16
		return v, binary.Read(r, binary.LittleEndian, &v)
	case gtInt16:
		var v int16
		return v, binary.Read(r, binary.LittleEndian, &v)
	case gtUint32:
		var v uint32
		return v, binary.Read(r, binary.LittleEndian, &v)
	case gtInt32:
		var v int32
		return v, binary.Read(r, binary.LittleEndian, &v)
	case gtFloat32:
		var v float32
		return v, binary.Read(r, binary.LittleEndian, &v)
	case gtBool:
		var v uint8
		err := binary.Read(r, binary.LittleEndian, &v)
		return v != 0, err
	case gtString:
		return readGGUFString(r, version)
	case gtUint64:
		var v uint64
		return v, binary.Read(r, binary.LittleEndian, &v)
	case gtInt64:
		var v int64
		return v, binary.Read(r, binary.LittleEndian, &v)
	case gtFloat64:
		var v float64
		return v, binary.Read(r, binary.LittleEndian, &v)
	case gtArray:
		var elemType uint32
		if err := binary.Read(r, binary.LittleEndian, &elemType); err != nil {
			return nil, err
		}
		var count uint64
		if version == 1 {
			var c32 uint32
			if err := binary.Read(r, binary.LittleEndian, &c32); err != nil {
				return nil, err
			}
			count = uint64(c32)
		} else {
			if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
				return nil, err
			}
		}
		// We don't need array contents for any capability check today; just
		// drain them so the stream stays aligned.
		for i := uint64(0); i < count; i++ {
			if _, err := readGGUFValue(r, version, ggufValueType(elemType)); err != nil {
				return nil, err
			}
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown gguf value type %d", t)
	}
}
