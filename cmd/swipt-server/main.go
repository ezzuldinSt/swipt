package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/swipt-automation/swipt/internal/computer"
	"github.com/swipt-automation/swipt/internal/log"

	"github.com/spf13/cobra"
)

const exitDeviceNotListed = 3

var (
	options     computer.Options
	pairOptions computer.PairOptions
	pairOutput  string
)

func main() {
	command := &cobra.Command{
		Use:          "swipt-server [-d NAME|UDID]",
		Short:        "Serve the Swipt API and MCP from this computer",
		Args:         cobra.NoArgs,
		RunE:         run,
		SilenceUsage: true,
	}
	command.Flags().StringVarP(&options.Device, "device", "d", "", "the device, by name or UDID")
	command.Flags().StringVar(&options.Passcode, "passcode", "", "unlock device with this passcode")
	command.Flags().StringVar(&options.PasscodeType, "passcode-type", "", "pin (4 or 6 digits, default), numeric or alphanumeric")
	command.Flags().StringVar(&options.DiskImage, "disk-image", "", "the directory of the Developer Disk Image to mount (default: Xcode's on macOS)")
	command.Flags().BoolVar(&options.Relay, "relay", false, "reach a device on Wi-Fi through usbmuxd's relay, not at its network address (also "+computer.RelayVariable+"=1)")
	pairCommand := &cobra.Command{
		Use:          "pair [-d NAME|UDID] [-o FILE]",
		Short:        "Create a remote pairing record for Swipt",
		Args:         cobra.NoArgs,
		RunE:         runPair,
		SilenceUsage: true,
	}
	pairCommand.Flags().StringVarP(&pairOptions.Device, "device", "d", "", "the device, by name or UDID")
	pairCommand.Flags().StringVarP(&pairOutput, "output", "o", "", "write record to FILE (default: stdout)")
	command.AddCommand(pairCommand)
	if guest := guestCommand(); guest != nil {
		command.AddCommand(guest)
	}
	if err := command.ExecuteContext(context.Background()); err != nil {
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		if errors.Is(err, computer.ErrDeviceNotListed) {
			os.Exit(exitDeviceNotListed)
		}
		os.Exit(1)
	}
}

func run(command *cobra.Command, _ []string) error {
	tuneMemory()
	options.Logs = log.NewFactory(log.Options{Level: log.LevelInfo, PlatformWriter: writer{}})
	options.Version = version()
	options.Output = command.OutOrStdout()
	ctx, stop := signal.NotifyContext(command.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	err := computer.Run(ctx, options)
	if err != nil && errors.Is(err, context.Canceled) && ctx.Err() != nil {
		command.SilenceErrors = true
	}
	return err
}

// tuneMemory keeps the Go heap near what the server holds unless GOGC or GOMEMLIMIT say
// otherwise. Its large allocations are frames and the pixels read from them, which live briefly,
// and at the default GOGC of 100 the heap grows to twice what is live before a collection: half
// keeps a script's loop of frames and text readings 35 MB smaller for a collection every few
// seconds. The limit makes the collector work harder before the Go runtime passes 512 MB.
func tuneMemory() {
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(50)
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(512 << 20)
	}
}

func runPair(command *cobra.Command, _ []string) error {
	pairOptions.Logs = log.NewFactory(log.Options{Level: log.LevelInfo, PlatformWriter: writer{}})
	ctx, stop := signal.NotifyContext(command.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	record, err := computer.Pair(ctx, pairOptions)
	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			command.SilenceErrors = true
		}
		return err
	}
	if pairOutput == "" {
		_, err = command.OutOrStdout().Write(record)
		return err
	}
	return os.WriteFile(pairOutput, record, 0o600)
}

func version() string {
	info, loaded := debug.ReadBuildInfo()
	if !loaded || info.Main.Version == "" {
		return "(devel)"
	}
	return info.Main.Version
}

type writer struct{}

func (writer) WriteMessage(level log.Level, message string) {
	fmt.Fprintln(os.Stderr, time.Now().Format("15:04:05.000"), levelName(level), message)
}

func levelName(level log.Level) string {
	switch level {
	case log.LevelError:
		return "ERROR"
	case log.LevelWarn:
		return "WARN"
	case log.LevelInfo:
		return "INFO"
	case log.LevelDebug:
		return "DEBUG"
	}
	return "TRACE"
}
