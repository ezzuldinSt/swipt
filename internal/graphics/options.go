package graphics

import (
	"github.com/swipt-automation/swipt/internal/device"
	"github.com/swipt-automation/swipt/internal/log"
)

type Options struct {
	Connection        *device.Connection
	Logger            log.ContextLogger
	OnRecordingFailed func(message string)
}
