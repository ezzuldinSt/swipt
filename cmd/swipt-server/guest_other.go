//go:build !(darwin && cgo)

package main

import "github.com/spf13/cobra"

// The guest commands set up a virtual iPhone, which runs on macOS only.
func guestCommand() *cobra.Command {
	return nil
}
