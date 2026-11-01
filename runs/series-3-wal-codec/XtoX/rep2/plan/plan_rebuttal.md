# Response to plan findings

## Major finding: attacker-controlled Batch count can cause unbounded allocation or work

**Finding:** A four-byte Batch payload can advertise `n = 0xffffffff` without containing child frames. Allocating `n` records before checking whether the payload can hold them could exhaust memory or panic instead of returning `ErrCorrupt`.

**Response:** Addressed in the revised plan's “Batch decoding and resource bounds” section and the Batch count mapping. Before allocating child storage or doing count-driven parsing, compute remaining payload bytes and reject when `n > remaining/9`, since the smallest complete frame is 9 bytes. Then parse children incrementally through a parser bounded to the Batch payload and require exactly `n` frames and exact consumption. Thus impossible attacker-provided counts fail as `ErrCorrupt` without an allocation proportional to the claimed count.
