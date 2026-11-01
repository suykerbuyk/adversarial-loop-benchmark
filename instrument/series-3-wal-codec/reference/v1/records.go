package walcodec

// Codecs for the v1 record types. Each is a small, self-contained pair of
// encode/decode functions registered into the dispatch table at init. Adding a
// record type means adding a file like this one — not touching the framing.

func init() {
	register(kindPut, recordCodec{encode: encodePut, decode: decodePut})
	register(kindDelete, recordCodec{encode: encodeDelete, decode: decodeDelete})
}

// Put payload = [klen][key][vlen][value].
func encodePut(rec Record, _ int) ([]byte, error) {
	p := rec.(Put)
	out := appendLenBytes(nil, []byte(p.Key))
	out = appendLenBytes(out, p.Value)
	return out, nil
}

func decodePut(payload []byte, _ int) (Record, error) {
	key, rest, err := readLenBytes(payload)
	if err != nil {
		return nil, err
	}
	val, rest, err := readLenBytes(rest)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, ErrCorrupt // trailing bytes in payload
	}
	return Put{Key: string(key), Value: val}, nil
}

// Delete payload = [klen][key].
func encodeDelete(rec Record, _ int) ([]byte, error) {
	d := rec.(Delete)
	return appendLenBytes(nil, []byte(d.Key)), nil
}

func decodeDelete(payload []byte, _ int) (Record, error) {
	key, rest, err := readLenBytes(payload)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, ErrCorrupt // trailing bytes in payload
	}
	return Delete{Key: string(key)}, nil
}
