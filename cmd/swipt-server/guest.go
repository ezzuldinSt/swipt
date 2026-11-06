//go:build darwin && cgo

package main

import (
	"context"

	"github.com/swipt-automation/swipt/internal/computer"

	"github.com/spf13/cobra"
)

func guestCommand() *cobra.Command {
	var udid, socket string
	group := &cobra.Command{Use: "guest", Hidden: true}
	group.PersistentFlags().StringVarP(&udid, "device", "d", "", "")
	group.PersistentFlags().StringVar(&socket, "socket", "", "")
	add := func(use string, args cobra.PositionalArgs, run func(context.Context, []string) error) {
		group.AddCommand(&cobra.Command{Use: use, Args: args, SilenceUsage: true, RunE: func(command *cobra.Command, args []string) error {
			return run(command.Context(), args)
		}})
	}
	add("pair", cobra.NoArgs, func(ctx context.Context, _ []string) error {
		return computer.GuestPair(ctx, udid, socket)
	})
	add("install ARCHIVE BUNDLE_ID", cobra.ExactArgs(2), func(ctx context.Context, args []string) error {
		return computer.GuestInstall(ctx, udid, args[0], args[1])
	})
	add("configure BUNDLE_ID NAME", cobra.ExactArgs(2), func(ctx context.Context, args []string) error {
		return computer.GuestConfigure(ctx, socket, args[0], args[1])
	})
	return group
}
