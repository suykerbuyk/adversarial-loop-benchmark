minor: The plan does not state whether Encode will preserve the baseline's support for *Put and *Delete values (including rejecting typed-nil pointers); those pointer types satisfy Record because Kind has a value receiver. Specify and preserve that behavior to avoid an accidental v1 API regression while adding Batch.
No blocker or major issue remains.
