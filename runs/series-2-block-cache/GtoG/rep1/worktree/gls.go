package blockcache

import "unsafe"

// The profiler-label pointer is a per-goroutine slot. Close uses it to
// recognize the flight goroutine and a Loader-context AfterFunc callback
// without parsing runtime.Stack. The two functions below are the linkname
// targets documented in runtime/proflabel.go.

//go:linkname runtime_setProfLabel runtime/pprof.runtime_setProfLabel
func runtime_setProfLabel(labels unsafe.Pointer)

//go:linkname runtime_getProfLabel runtime/pprof.runtime_getProfLabel
func runtime_getProfLabel() unsafe.Pointer

// reenterMark is stored as the current goroutine's profiler labels while that
// goroutine is inside a Loader call or a callback this cache started for the
// Loader context. It is a distinct allocation so it cannot collide with a
// label map a user installed. The previous pointer is restored on the way out,
// so a Loader that itself uses pprof labels is not left with this mark.
var reenterMark uintptr = 0xB10C

func markReenter() (prev unsafe.Pointer) {
	prev = runtime_getProfLabel()
	runtime_setProfLabel(unsafe.Pointer(&reenterMark))
	return prev
}

func unmarkReenter(prev unsafe.Pointer) {
	runtime_setProfLabel(prev)
}

func reenterMarked() bool {
	p := runtime_getProfLabel()
	return p == unsafe.Pointer(&reenterMark)
}
