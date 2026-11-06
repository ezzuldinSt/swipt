package feed

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	avrt                                = windows.NewLazySystemDLL("avrt.dll")
	procAvSetMmThreadCharacteristicsW   = avrt.NewProc("AvSetMmThreadCharacteristicsW")
	procAvRevertMmThreadCharacteristics = avrt.NewProc("AvRevertMmThreadCharacteristics")
	kernel32                            = windows.NewLazySystemDLL("kernel32.dll")
	procSetThreadPriority               = kernel32.NewProc("SetThreadPriority")
)

const (
	threadPriorityNormal  = 0
	threadPriorityHighest = 2
)

// playbackTask is the Multimedia Class Scheduler Service task that Media Foundation's own
// playback threads join.
var playbackTask, _ = windows.UTF16PtrFromString("Playback")

// raiseThreadPriority puts the calling thread, which the caller keeps its goroutine on, in
// MMCSS's Playback task, or at THREAD_PRIORITY_HIGHEST when the service refuses it, so the
// decoding keeps up while the conversions and OCR take every processor; it answers how, and
// restore puts the thread back before it returns to Go's other goroutines.
func raiseThreadPriority() (string, func()) {
	if procAvSetMmThreadCharacteristicsW.Find() == nil && procAvRevertMmThreadCharacteristics.Find() == nil {
		var index uint32
		task, _, _ := syscall.SyscallN(procAvSetMmThreadCharacteristicsW.Addr(), uintptr(unsafe.Pointer(playbackTask)), uintptr(unsafe.Pointer(&index)))
		if task != 0 {
			return "MMCSS Playback", func() {
				syscall.SyscallN(procAvRevertMmThreadCharacteristics.Addr(), task)
			}
		}
	}
	thread := windows.CurrentThread()
	if ok, _, _ := syscall.SyscallN(procSetThreadPriority.Addr(), uintptr(thread), threadPriorityHighest); ok != 0 {
		return "highest", func() {
			syscall.SyscallN(procSetThreadPriority.Addr(), uintptr(thread), threadPriorityNormal)
		}
	}
	return "normal", func() {}
}
