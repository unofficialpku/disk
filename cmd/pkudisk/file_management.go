package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/display"
)

const confirmationHelp = `Destructive operations ask for confirmation in a terminal. -y/--yes skips
that confirmation. --non-interactive requires --yes for destructive operations.
Missing required arguments are prompted only in an interactive terminal.`

func requiredInputs(cmd *cobra.Command, args []string, nonInteractive bool, labels ...string) ([]string, error) {
	values := append([]string{}, args...)
	for len(values) < len(labels) {
		label := labels[len(values)]
		if nonInteractive || !interactiveIO(cmd.InOrStdin(), cmd.ErrOrStderr()) {
			return nil, usageFailure(cmd, "missing "+label+"; supply it explicitly or run interactively")
		}
		value, err := ttyInput(cmd.Context(), label+": ", false)
		if err != nil {
			return nil, err
		}
		if len(value) == 0 {
			return nil, usageFailure(cmd, label+" cannot be empty")
		}
		values = append(values, string(value))
	}
	return values, nil
}

func checkConfirmation(cmd *cobra.Command, nonInteractive, yes bool) error {
	if !yes && (nonInteractive || !interactiveIO(cmd.InOrStdin(), cmd.ErrOrStderr())) {
		return usageFailure(cmd, "confirmation requires a terminal; use --yes to explicitly approve this operation")
	}
	return nil
}

func confirmOperation(cmd *cobra.Command, yes bool, description string) error {
	if yes {
		return nil
	}
	value, err := ttyInput(cmd.Context(), description+" [y/N]: ", false)
	if err != nil {
		return err
	}
	if answer := strings.ToLower(string(value)); answer != "y" && answer != "yes" {
		return errors.New("operation cancelled; no changes made")
	}
	return nil
}

func newDeleteCommand() *cobra.Command {
	var auth commandAuth
	var recursive, yes bool
	cmd := &cobra.Command{Use: "delete [REMOTE-PATH]", Short: "Delete a remote file or directory into its recycle bin", Args: cobra.MaximumNArgs(1),
		Long: `Delete a remote file using the server's recycle policy. Directories require
-r/--recursive and delete their entire subtree. Library roots cannot be deleted.
Use 'recycle ls' and 'recycle restore' to find and restore deleted objects while
server retention permits it. This differs from 'recycle remove', which is permanent.

` + confirmationHelp + "\n\n" + remoteHelp,
		Example: "  pkudisk delete '/My library/report.pdf'\n  pkudisk delete '/My library/Old photos' -r\n  pkudisk delete '/My library/temp.txt' --non-interactive -y",
		RunE: func(cmd *cobra.Command, args []string) error {
			args, err := requiredInputs(cmd, args, auth.credentials.nonInteractive, "REMOTE-PATH")
			if err != nil {
				return err
			}
			if err := checkConfirmation(cmd, auth.credentials.nonInteractive, yes); err != nil {
				return err
			}
			client, closer, err := auth.open(cmd)
			if err != nil {
				return err
			}
			defer closer.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), auth.timeout)
			defer cancel()
			entry, err := client.Resolve(ctx, args[0])
			if err != nil {
				return err
			}
			if err := anyshare.ValidateDelete(entry); err != nil {
				return err
			}
			if entry.Dir && !recursive {
				return usageFailure(cmd, "deleting a directory requires --recursive")
			}
			if err := confirmOperation(cmd, yes, "Delete "+entry.Name+" ("+entry.ID+") into the recycle bin?"); err != nil {
				return err
			}
			if err := client.Delete(ctx, entry); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s (%s). Inspect with 'recycle ls'.\n", entry.Name, entry.ID)
			return nil
		},
	}
	auth.add(cmd)
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "delete a directory and all of its contents")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "approve deletion without a confirmation prompt")
	return cmd
}

func newRecycleCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "recycle", Short: "List, restore or permanently remove deleted remote objects",
		Long: "Manage the server recycle bin, separate from uncommitted local writes shown by recover.\nListing defaults to all owned libraries of the selected account. A LIBRARY\nargument selects one accessible library root by path or gns:// ID. Recycle\npermissions and retention are controlled by the server. Object IDs are shown\nby 'recycle ls'; deleted paths cannot be resolved with ordinary ls.\n\n" + confirmationHelp,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	for _, action := range []string{"ls", "restore", "remove", "empty"} {
		cmd.AddCommand(newRecycleOperation(action))
	}
	return cmd
}

func recycleLibraries(ctx context.Context, client *anyshare.Client, selector string) ([]anyshare.Entry, error) {
	if selector == "" {
		return client.RecycleLibraries(ctx)
	}
	entry, err := client.Resolve(ctx, selector)
	if err != nil {
		return nil, err
	}
	if entry.ID != anyshare.LibraryID(entry.ID) {
		return nil, errors.New("recycle scope must be a library root, not a file or subfolder")
	}
	return []anyshare.Entry{entry}, nil
}

func findRecycled(ctx context.Context, client *anyshare.Client, id string) (anyshare.RecycleEntry, error) {
	if anyshare.LibraryID(id) == "" || anyshare.ParentID(id) == "" {
		return anyshare.RecycleEntry{}, errors.New("use a recycled gns:// object ID from 'recycle ls'")
	}
	entries, err := client.RecycleList(ctx, anyshare.LibraryID(id))
	if err != nil {
		return anyshare.RecycleEntry{}, err
	}
	for _, entry := range entries {
		if entry.ID == id {
			return entry, nil
		}
	}
	return anyshare.RecycleEntry{}, fmt.Errorf("%w: recycled object %s", anyshare.ErrNotFound, id)
}

func newRecycleOperation(action string) *cobra.Command {
	var auth commandAuth
	var sizes sizeFormat
	var asJSON, rename, yes bool
	cmd := &cobra.Command{Use: action + " [RECYCLE-ID]", Args: cobra.MaximumNArgs(1)}
	switch action {
	case "ls":
		cmd.Use, cmd.Short = "ls [LIBRARY]", "List all deleted files/folders, their original paths and IDs"
		cmd.Long = "Read every recycle page from all owned libraries, or one selected library.\nDELETED is the server's deletion timestamp. Folder sizes show -. JSON keeps\noriginal metadata and numeric byte sizes. No files are changed."
	case "restore":
		cmd.Short = "Restore one recycled object to its original location"
		cmd.Long = "Restore an ID from 'recycle ls' to its original location. The default refuses\na name collision; --rename lets the server choose a new name and keeps both\nobjects. Restoring a directory restores its subtree. Server permissions,\nretention and quota apply."
	case "remove":
		cmd.Short = "Permanently delete one recycled object (cannot be undone)"
		cmd.Long = "Permanently delete an ID from 'recycle ls', including a directory's entire\nsubtree. This cannot be restored. Only objects currently in the bin are accepted."
	case "empty":
		cmd.Use, cmd.Short = "empty [LIBRARY]", "Permanently remove the listed recycle contents (cannot be undone)"
		cmd.Long = "Snapshot all owned libraries' recycle contents, or one selected library,\nthen permanently delete those entries after confirmation. Newly deleted objects\nare excluded from this snapshot. Changes are per object; failures report how many\nwere already removed. There is no rollback."
	}
	cmd.Long += "\n\n" + confirmationHelp
	cmd.Example = "  pkudisk recycle " + action + " 'gns://LIBRARY/OBJECT-ID'"
	if action == "ls" || action == "empty" {
		cmd.Example = "  pkudisk recycle " + action + "\n  pkudisk recycle " + action + " '/My library'"
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if action == "restore" || action == "remove" {
			var err error
			args, err = requiredInputs(cmd, args, auth.credentials.nonInteractive, "RECYCLE-ID")
			if err != nil {
				return err
			}
		}
		if action == "restore" || action == "remove" || action == "empty" {
			if err := checkConfirmation(cmd, auth.credentials.nonInteractive, yes); err != nil {
				return err
			}
		}
		client, closer, err := auth.open(cmd)
		if err != nil {
			return err
		}
		defer closer.Close()
		ctx, cancel := context.WithTimeout(cmd.Context(), auth.timeout)
		defer cancel()
		if action == "restore" || action == "remove" {
			entry, err := findRecycled(ctx, client, args[0])
			if err != nil {
				return err
			}
			if action == "restore" {
				if err := confirmOperation(cmd, yes, "Restore "+entry.Name+" to "+entry.Path+"?"); err != nil {
					return err
				}
				result, err := client.RecycleRestore(ctx, entry.ID, rename)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Restored %s -> %s\n", entry.Name, result.ID)
				return nil
			}
			if err := confirmOperation(cmd, yes, "Permanently remove "+entry.Name+" ("+entry.ID+")? This cannot be undone."); err != nil {
				return err
			}
			if err := client.RecycleDelete(ctx, entry.ID); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Permanently removed %s (%s)\n", entry.Name, entry.ID)
			return nil
		}
		selector := ""
		if len(args) > 0 {
			selector = args[0]
		}
		libraries, err := recycleLibraries(ctx, client, selector)
		if err != nil {
			return err
		}
		entries, err := client.RecycleSnapshot(ctx, libraries)
		if err != nil {
			return err
		}
		if action == "empty" {
			if len(entries) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Recycle bin is empty.")
				return nil
			}
			if err := printRecycle(cmd.ErrOrStderr(), entries, false, sizes); err != nil {
				return err
			}
			if err := confirmOperation(cmd, yes, fmt.Sprintf("Permanently remove these %d recycled objects? This cannot be undone.", len(entries))); err != nil {
				return err
			}
			if _, err := client.RecycleEmpty(ctx, entries, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Permanently removed %d recycled objects.\n", len(entries))
			return nil
		}
		return printRecycle(cmd.OutOrStdout(), entries, asJSON, sizes)
	}
	auth.add(cmd)
	if action == "ls" || action == "empty" {
		sizes.add(cmd)
	}
	if action == "ls" {
		cmd.Flags().BoolVarP(&asJSON, "json", "j", false, "write all recycle entries as a JSON array")
	}
	if action == "restore" {
		cmd.Flags().BoolVar(&rename, "rename", false, "choose a new name on a collision, retaining both objects")
	}
	if action == "restore" || action == "remove" || action == "empty" {
		cmd.Flags().BoolVarP(&yes, "yes", "y", false, "approve this operation without a confirmation prompt")
	}
	return cmd
}

func printRecycle(out io.Writer, entries []anyshare.RecycleEntry, asJSON bool, sizes sizeFormat) error {
	return display.RenderList(out, entries, display.ListOptions[anyshare.RecycleEntry]{
		JSON: asJSON, Headers: []string{"NAME", "TYPE", "SIZE", "DELETED", "ORIGINAL PATH", "ID"}, Numbers: []int{2},
		Row: func(entry anyshare.RecycleEntry) []string {
			size, kind := "-", "folder"
			if !entry.Dir {
				kind = "file"
				if entry.Size >= 0 {
					size = sizes.value(entry.Size)
				}
			}
			return []string{entry.Name, kind, size, displayTime(entry.Modified), entry.Path, entry.ID}
		},
	})
}

func displayTime(value time.Time) string {
	if value.IsZero() {
		return "-"
	}
	return value.Local().Format(time.RFC3339)
}

func newRevisionCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "revision", Short: "Inspect or restore a remote file's version history",
		Long: "Version history belongs to a remote file. 'revision ls' shows versions;\n'revision restore' changes its current contents to a selected historical\nversion. Library retention and permissions determine available versions.",
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	for _, action := range []string{"ls", "restore"} {
		cmd.AddCommand(newRevisionOperation(action))
	}
	return cmd
}

func newRevisionOperation(action string) *cobra.Command {
	var auth commandAuth
	var sizes sizeFormat
	var asJSON, yes bool
	cmd := &cobra.Command{Use: action + " [REMOTE-FILE]", Args: cobra.MaximumNArgs(1), Short: "List a remote file's versions, sizes, editors and timestamps",
		Long: "Read this file's retained versions. REV is the identifier passed to\n'revision restore'; CURRENT marks the current version. Folders are rejected.\nJSON keeps numeric byte sizes. No files are changed.",
	}
	if action == "restore" {
		cmd.Use, cmd.Args = "restore [REMOTE-FILE] [REV]", cobra.MaximumNArgs(2)
		cmd.Short = "Make a historical version the current file contents"
		cmd.Long = "Restore a REV from 'revision ls REMOTE-FILE'. This changes the selected\nfile's current content on the server without transferring file bytes. Folders\nare rejected. Available history and retention are controlled by the server.\n\n" + confirmationHelp
	}
	cmd.Long += "\n\n" + remoteHelp
	cmd.Example = "  pkudisk revision " + action + " '/My library/report.pdf'"
	if action == "restore" {
		cmd.Example += " REV\n  pkudisk revision restore '/My library/report.pdf' REV --non-interactive -y"
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		labels := []string{"REMOTE-FILE"}
		if action == "restore" {
			labels = append(labels, "REV")
		}
		args, err := requiredInputs(cmd, args, auth.credentials.nonInteractive, labels...)
		if err != nil {
			return err
		}
		if action == "restore" {
			if err := checkConfirmation(cmd, auth.credentials.nonInteractive, yes); err != nil {
				return err
			}
		}
		client, closer, err := auth.open(cmd)
		if err != nil {
			return err
		}
		defer closer.Close()
		ctx, cancel := context.WithTimeout(cmd.Context(), auth.timeout)
		defer cancel()
		entry, err := client.Resolve(ctx, args[0])
		if err != nil {
			return err
		}

		if action == "ls" {
			versions, err := client.Revisions(ctx, entry)
			if err != nil {
				return err
			}
			return display.RenderList(cmd.OutOrStdout(), versions, display.ListOptions[anyshare.Revision]{
				JSON: asJSON, Headers: []string{"CURRENT", "REV", "SIZE", "MODIFIED", "EDITOR", "NAME"}, Numbers: []int{2},
				Row: func(revision anyshare.Revision) []string {
					current := ""
					if revision.Rev == entry.Rev {
						current = "*"
					}
					return []string{current, revision.Rev, sizes.value(revision.Size), displayTime(revision.Modified), revision.Editor, revision.Name}
				},
			})
		}
		revision, err := client.Revision(ctx, entry, args[1])
		if err != nil {
			return err
		}
		if err := confirmOperation(cmd, yes, "Restore "+entry.Name+" to revision "+revision.Rev+" ("+displayTime(revision.Modified)+")?"); err != nil {
			return err
		}
		if err := client.RestoreRevision(ctx, entry, revision.Rev); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Restored %s to revision %s\n", entry.ID, revision.Rev)
		return nil
	}
	auth.add(cmd)
	if action == "ls" {
		sizes.add(cmd)
		cmd.Flags().BoolVarP(&asJSON, "json", "j", false, "write versions as a JSON array")
	} else {
		cmd.Flags().BoolVarP(&yes, "yes", "y", false, "approve version restoration without a confirmation prompt")
	}
	return cmd
}
