package computer

import (
	"unsafe"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/log"

	"golang.org/x/sys/windows"
)

// The server mostly runs unseen, started at logon by a scheduled task, which Task Scheduler
// starts below normal priority and Windows 11 then throttles as a background process: its
// threads go to efficiency mode and its timers to 15.6 ms. The screen stream fell from 42 to
// 16 frames a second that way, its frames seconds old, so the server opts out of both.
const (
	processPowerThrottling         = 4 // PROCESS_INFORMATION_CLASS ProcessPowerThrottling
	powerThrottlingCurrentVersion  = 1
	powerThrottlingExecutionSpeed  = 0x1
	powerThrottlingIgnoreTimerRate = 0x4
)

var setProcessInformation = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetProcessInformation")

func runInForeground(logger log.ContextLogger) {
	process := windows.CurrentProcess()
	if err := windows.SetPriorityClass(process, windows.NORMAL_PRIORITY_CLASS); err != nil {
		logger.Debug(E.Cause(err, "raise the priority to normal"))
	}
	// PROCESS_POWER_THROTTLING_STATE: controlled but not set means never throttled.
	state := struct {
		Version, ControlMask, StateMask uint32
	}{powerThrottlingCurrentVersion, powerThrottlingExecutionSpeed | powerThrottlingIgnoreTimerRate, 0}
	if ok, _, err := setProcessInformation.Call(uintptr(process), processPowerThrottling, uintptr(unsafe.Pointer(&state)), unsafe.Sizeof(state)); ok == 0 {
		logger.Debug(E.Cause(err, "opt out of power throttling"))
	}
}
