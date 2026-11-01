// Package walcodec encodes a sequence of write-ahead-log records to a framed
// byte stream and decodes it back.
//
// # Wire format (version 2)
//
// All integers are big-endian.
//
//	stream = version(1) frame*
//	frame  = len(4) type(1) payload(len-1) crc(4)
//
// version is 1 or 2; Encode always writes 2. Any other version is rejected
// before any frame is read. A version-1 stream may contain only types 1 and
// 2; a version-2 stream may contain all three types.
//
// len is a uint32 counting type+payload only; the trailing crc lies outside
// len. crc is the CRC-32 (IEEE) of type+payload. len is capped at 16 MiB
// (16<<20) for every frame at every depth; a larger len is rejected before
// anything sized by it is read or allocated.
//
// Payloads, where strings and byte slices are written as [uint32 len][bytes]:
//
//	Put    (type 1) = [klen uint32][key][vlen uint32][value]
//	Delete (type 2) = [klen uint32][key]
//	Batch  (type 3) = [n uint32] frame{n}
//
// A Batch's inner frames are full frames, crcs included, and are counted in
// the outer len. They must consume the Batch payload exactly; fewer or more
// frames, or trailing bytes, are corrupt. An inner frame may not extend past
// its enclosing payload.
//
// Depth: the top-level stream is depth 0, and a Batch's sub-stream is one
// level deeper than the stream holding the Batch. A sub-stream deeper than 8
// is rejected by Decode, and Encode refuses to produce one.
//
// A nil Value and an empty Value both encode as vlen = 0 and both decode to a
// non-nil, length-0 []byte. A decoded Batch has non-nil Records.
//
// Every Decode failure wraps ErrCorrupt; test for it with errors.Is, not ==.
// Decode errors name the absolute byte offset of the failing frame and the
// depth of the stream that holds it. Encode errors start with "walcodec: "
// and do not wrap ErrCorrupt.
package walcodec
