minor: The plan copies resident bytes while holding the single cache mutex on every hit; large blocks therefore serialize unrelated hits, Len calls, completions, and Close behind memory copies. Capture the immutable cache-owned slice under lock, then copy it after unlocking to keep the critical section bounded.
No blocker or major issue remains based on the spec and public tests.
