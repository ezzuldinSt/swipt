//go:build !windows

package computer

import "github.com/swipt-automation/swipt/internal/log"

func runInForeground(log.ContextLogger) {}
