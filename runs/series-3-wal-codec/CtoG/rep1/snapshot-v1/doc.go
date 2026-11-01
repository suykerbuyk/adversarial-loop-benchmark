// Package walcodec encodes a sequence of write-ahead-log records to a framed
// byte stream and decodes it back.
//
// # Wire format (version 1)
//
// All integers are big-endian.
//
//	stream = version(1) frame*
//	frame  = len(4) type(1) payload(len-1) crc(4)
//
// len is a uint32 counting type+payload only; the trailing crc lies outside
// len. crc is the CRC-32 (IEEE) of type+payload. len is capped at 16 MiB
// (16<<20); a larger len is rejected before anything is allocated.
//
// Payloads, where strings and byte slices are written as [uint32 len][bytes]:
//
//	Put    (type 1) = [klen uint32][key][vlen uint32][value]
//	Delete (type 2) = [klen uint32][key]
//
// A nil Value and an empty Value both encode as vlen = 0 and both decode to a
// non-nil, length-0 []byte.
//
// Every Decode failure wraps ErrCorrupt; test for it with errors.Is, not ==.
package walcodec
