# Response to plan findings

## Major: depth check timing and boundary

Addressed in the revised plan. It now states the check occurs before processing a sub-stream: prospective depth greater than 8 returns the appropriate error. Top-level is depth 0, an inner Batch stream is one level deeper, and depth 8 is allowed only for Put/Delete records. Encode mirrors the same check, accepting at most eight Batch levels and rejecting a ninth.

## Major: complete encoded-size accounting

Addressed in the revised plan. It specifies the outer body size as `1 + 4 + sum(4 + innerBodyLen + 4)`: one outer type byte, the count field, and every inner frame's length, body, and CRC. This complete body must be at most 16 MiB before allocating or emitting the outer frame, with the same cap checked recursively for every frame. Decode is explicitly bounded by the enclosing Batch payload slice.

## Minor: version-1 Batch kind and version-2 output

Addressed in the revised plan. Encode emits version 2 for every input, including empty streams. Version 1 decoding accepts only Put/Delete. A kind-3 frame under version 1 is CRC-checked and then rejected as an unknown kind without parsing its payload recursively. Unsupported versions are rejected before frames are parsed.

## Minor: encode versus decode error classes

Addressed in the revised plan. Invalid records, nil pointers/records, unknown types, oversized fields or frame bodies, and excessive depth use the existing encode-error sentinel without wrapping `ErrCorrupt`. Malformed bytes and unsupported versions/kinds return `ErrCorrupt` at every depth.

## Nit: empty Batch encoding

Addressed in the revised plan. Both nil and empty `Batch.Records` encode with a present 4-byte zero count and decode to a Batch with zero records; the count field is required and consumes the payload exactly.
