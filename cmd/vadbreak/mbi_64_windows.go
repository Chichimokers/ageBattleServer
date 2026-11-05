//go:build windows && !386 && !arm

package main

import "unsafe"

// memBasicInformation is MEMORY_BASIC_INFORMATION for a 64-bit Windows target:
// 48 bytes, with RegionSize at offset 16. The two alignment fields are explicit
// so the size does not depend on how the compiler pads the structure.
type memBasicInformation struct {
	BaseAddress       uintptr
	AllocationBase    uintptr
	AllocationProtect uint32
	_                 uint32
	RegionSize        uintptr
	State             uint32
	Protect           uint32
	Type              uint32
	_                 uint32
}

// Compile-time layout assertions: a wrong size or offset must fail the build.
var (
	_ = [1]struct{}{}[48-int(unsafe.Sizeof(memBasicInformation{}))]
	_ = [1]struct{}{}[24-int(unsafe.Offsetof(memBasicInformation{}.RegionSize))]
)
