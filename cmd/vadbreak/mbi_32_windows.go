//go:build windows && (386 || arm)

package main

import "unsafe"

// memBasicInformation is MEMORY_BASIC_INFORMATION for a 32-bit Windows target:
// 28 bytes, with RegionSize at offset 12. The 64-bit layout used to be compiled
// on 386 as well, which made VirtualQueryEx write 28 bytes into a 36-byte
// structure and the walk read State/Protect as RegionSize.
type memBasicInformation struct {
	BaseAddress       uintptr
	AllocationBase    uintptr
	AllocationProtect uint32
	RegionSize        uintptr
	State             uint32
	Protect           uint32
	Type              uint32
}

// Compile-time layout assertions: a wrong size or offset must fail the build,
// not hang the tool at runtime.
var (
	_ = [1]struct{}{}[28-int(unsafe.Sizeof(memBasicInformation{}))]
	_ = [1]struct{}{}[12-int(unsafe.Offsetof(memBasicInformation{}.RegionSize))]
)
