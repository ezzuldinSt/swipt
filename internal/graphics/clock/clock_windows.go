package clock

import (
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32                      = windows.NewLazySystemDLL("kernel32.dll")
	procQueryPerformanceCounter   = kernel32.NewProc("QueryPerformanceCounter")
	procQueryPerformanceFrequency = kernel32.NewProc("QueryPerformanceFrequency")
	frequency                     int64
)

func init() {
	// Windows XP and later always have a performance counter, and its frequency is fixed at boot.
	if err := procQueryPerformanceFrequency.Find(); err == nil {
		syscall.SyscallN(procQueryPerformanceFrequency.Addr(), uintptr(unsafe.Pointer(&frequency)))
	}
}

var started = time.Now()

// ticks reads QueryPerformanceCounter in nanoseconds.
func ticks() int64 {
	if frequency <= 0 {
		return int64(time.Since(started))
	}
	var counter int64
	syscall.SyscallN(procQueryPerformanceCounter.Addr(), uintptr(unsafe.Pointer(&counter)))
	return counter/frequency*int64(time.Second) + counter%frequency*int64(time.Second)/frequency
}

// now reads GetSystemTimePreciseAsFileTime, the system time moved on by the performance
// counter since the last timer tick.
func now() float64 {
	var filetime windows.Filetime
	windows.GetSystemTimePreciseAsFileTime(&filetime)
	return float64(filetime.Nanoseconds()) / 1e6
}
