package main

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/unofficialpku/disk/internal/desktop"
	"github.com/unofficialpku/disk/internal/gui"
	"github.com/unofficialpku/disk/internal/localfile"
)

// Only known desktop launches change the empty invocation. In particular,
// missing terminal handles do not turn scripts, pipes, or SSH into a GUI.
func launchArgs(args []string, desktopLaunch bool) []string {
	if len(args) == 0 && desktopLaunch {
		return []string{"gui"}
	}
	return args
}

func newGUICommand() *cobra.Command {
	var foreground bool
	cmd := &cobra.Command{
		Use: "gui", Short: "Open the desktop tray and bilingual browser interface",
		Long: `Open the PKUDisk desktop interface in your default browser. Manage saved
accounts, browse/search files, transfer and mount, manage sharing, recycle-bin
contents and file history, and inspect retained mount writes.

Only IPv4 loopback is used. Each browser launch receives a one-use login link.
Closing the browser keeps the service running. Run gui again to reopen the
existing instance. Run gui stop or use the tray/page Exit action to stop it. On Linux/macOS,
terminal launches run in the background and print a private log path. Use
--foreground to keep the service attached to the terminal; Ctrl-C then stops it.
Desktop launchers keep their native app lifecycle. If the Linux desktop
has no system tray, the browser page provides the Exit action.

A graphical session and a configured default browser are required. Only
mounting needs a filesystem driver. Help and completion stay offline.`,
		Example: "  pkudisk gui\n  pkudisk gui stop\n  pkudisk gui --foreground\n  pkudisk gui --help",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := localfile.ConfigDir()
			if err != nil {
				return err
			}
			return gui.RunMode(cmd.Context(), gui.Options{
				Directory: filepath.Join(dir, "pkudisk", "gui"),
				Version:   installedVersion(), Output: cmd.ErrOrStderr(),
				OpenBrowser:    desktop.OpenBrowser,
				NewApplication: newGUIApplication,
				NewTray: func(open, quit func()) (gui.Tray, error) {
					tray, err := desktop.New(open, quit)
					if err != nil {
						return nil, err
					}
					return tray, nil
				},
			}, foreground, desktop.StartedFromDesktop())
		},
	}
	cmd.Flags().BoolVar(&foreground, "foreground", false, "Keep Linux/macOS GUI attached to the terminal (Ctrl-C stops it)")
	cmd.AddCommand(&cobra.Command{
		Use: "stop", Short: "Stop the running GUI after active work has finished", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := localfile.ConfigDir()
			if err != nil {
				return err
			}
			stopped, err := gui.Stop(cmd.Context(), filepath.Join(dir, "pkudisk", "gui"))
			if err != nil {
				return err
			}
			if stopped {
				fmt.Fprintln(cmd.OutOrStdout(), "GUI stopped.")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "GUI is not running.")
			}
			return nil
		},
	})
	return cmd
}
