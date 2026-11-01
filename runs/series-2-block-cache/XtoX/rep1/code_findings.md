minor: `clone` converts a non-nil zero-length loader result into nil (`append([]byte(nil), b...)` returns nil when `b` is empty), so a successful `Get` does not preserve the loader's empty-slice result. This is observable to callers (`got == nil`) and conflicts with returning the loaded result as a fresh copy; preserve non-nil emptiness when cloning.
No blocker or major issue remains.
