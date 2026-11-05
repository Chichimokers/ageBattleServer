//go:build windows

package main

import (
	"fmt"
	"strconv"
	"syscall"
	"unsafe"
)

const processQueryInformation = 0x0400
const processVMSize = 0x0008

// memBasicInformation is defined per pointer width in mbi_32_windows.go and
// mbi_64_windows.go: the 32-bit structure is 28 bytes and the 64-bit one 48,
// and using the wider layout on a 32-bit build reads RegionSize from the wrong
// offset and makes the walk advance through State/Protect instead.

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procOpenProcess  = kernel32.NewProc("OpenProcess")
	procCloseHandle  = kernel32.NewProc("CloseHandle")
	procVirtualQuery = kernel32.NewProc("VirtualQueryEx")
	procSystemInfo   = kernel32.NewProc("GetNativeSystemInfo")
	procProcessMem   = kernel32.NewProc("K32GetProcessMemoryInfo")

	// The counters come from kernel32 and not from the psapi forwarder, which refuses
	// every cb on this machine, and cb is the plain structure's own size rather
	// than a size derived from a pointer-width field: 72 bytes on 64-bit Windows
	// and 40 on 32-bit. A fixed 72 on a 32-bit build makes the call refuse the
	// structure and the commit charge come out as unknown. Both failures are
	// invisible as a wrong number because each returns a zeroed structure, which
	// is why CommitChargeKnown is carried separately from the value.
	// Kept in sync with cmd/benchpeer/procinfo_windows.go.
	errNoPrivateUsage = fmt.Errorf("K32GetProcessMemoryInfo returned a zeroed structure")
)

type processMemoryCounters struct {
	cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// sizeOfProcessMemoryCounters is derived from the struct itself: 72 bytes on
// 64-bit Windows and 40 on 32-bit. The API refuses a cb that does not match its
// own structure, and the refusal is a zeroed structure, not an error code.
const sizeOfProcessMemoryCounters = unsafe.Sizeof(processMemoryCounters{})

// systemInfo is the SYSTEM_INFO block GetNativeSystemInfo writes. It is kept as
// a fixed-size byte block because the structure is larger than the fields this
// tool reads: the API writes the whole thing, and a shorter Go struct makes the
// call overwrite whatever follows it on the stack. On 64-bit Windows the block
// is 48 bytes and the 32-bit one 36; 48 covers both, and pageSize is the DWORD
// at offset 4. Reading it through a pointer-sized field would land on
// lpMinimumApplicationAddress instead, which is how a report ends up stepping
// over 64 KB at a time wherever it cannot describe a region.
type systemInfo struct {
	_ [48]byte
}

func pageSize() uintptr {
	var si systemInfo
	procSystemInfo.Call(uintptr(unsafe.Pointer(&si)))
	pageSize := uintptr(*(*uint32)(unsafe.Add(unsafe.Pointer(&si), 4)))
	if pageSize == 0 {
		return 4096
	}
	return pageSize
}

// commitChargeBytes reads the figure benchpeer reports, so one run of this tool and one
// run of benchpeer are talking about the same number and the two can be reconciled.
//
// It is PagefileUsage, the process's commit charge, and not PrivateUsage: PrivateUsage
// lives in PROCESS_MEMORY_COUNTERS_EX and not in the plain 72-byte structure, and the
// counters for this process have to come from kernel32 with cb set to the plain structure's
// 72 bytes. The column benchpeer labels "commit / private" is therefore the commit charge.
// The quota fields are a third figure again and are not this one.
func commitChargeBytes(handle syscall.Handle) (uint64, bool) {
	var pmc processMemoryCounters
	pmc.cb = uint32(sizeOfProcessMemoryCounters)
	ret, _, _ := procProcessMem.Call(uintptr(handle), uintptr(unsafe.Pointer(&pmc)), sizeOfProcessMemoryCounters)
	if ret == 0 {
		return 0, false
	}
	if pmc.PagefileUsage == 0 {
		return 0, false
	}
	return uint64(pmc.PagefileUsage), true
}

// walk totals the committed and reserved-only bytes of a process's address space by
// region type and protection.
//
// Two limits bound the walk and both are properties of the request, not of the target: a
// 64-bit Windows address space is walked up to 2^47, and a region that VirtualQueryEx
// declines to describe is stepped over one granule at a time rather than ending the walk,
// because ending early would silently under-report.
func walk(pid int) (*Report, error) {
	handle, _, err := procOpenProcess.Call(processQueryInformation|processVMSize, 0, uintptr(pid))
	if handle == 0 {
		return nil, fmt.Errorf("OpenProcess(%d): %w", pid, err)
	}
	defer procCloseHandle.Call(handle)

	rep := &Report{PID: pid, PageSize: uint64(pageSize())}
	if v, ok := commitChargeBytes(syscall.Handle(handle)); ok {
		rep.CommitChargeBytes = v
		rep.CommitChargeKnown = true
	}

	gran := pageSize()
	var addr uintptr
	// 64-bit Windows is walked up to 2^47; a 32-bit address space has no such
	// address, and the constant does not fit a uintptr there, so 386 walks the
	// whole address space instead.
	limit := ^uintptr(0)
	if strconv.IntSize == 64 {
		// A variable shift, so the 2^47 constant is not evaluated as a uintptr
		// on a 32-bit build and rejected at compile time.
		var highBit uint = 47
		limit = uintptr(1) << highBit
	}
	for addr < limit {
		var mbi memBasicInformation
		ret, _, _ := procVirtualQuery.Call(handle, addr, uintptr(unsafe.Pointer(&mbi)), unsafe.Sizeof(mbi))
		if ret == 0 || mbi.RegionSize == 0 {
			next := addr + gran
			if next <= addr {
				// The address space wrapped: there is nothing above this.
				break
			}
			addr = next
			continue
		}
		rep.add(regionTypeName(mbi.Type), protectionName(mbi.Protect), mbi.State, uint64(mbi.RegionSize))
		next := mbi.BaseAddress + mbi.RegionSize
		if next <= addr {
			// The top region ends at the top of the address space and its end
			// wraps to 0. Continuing would walk the same space from the start.
			break
		}
		addr = next
	}
	rep.finish()
	return rep, nil
}
