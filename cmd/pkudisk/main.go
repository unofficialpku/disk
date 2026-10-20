package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/desktop"
	"github.com/unofficialpku/disk/internal/diagnostic"
	"github.com/unofficialpku/disk/internal/display"
	"github.com/unofficialpku/disk/internal/localfile"
	mountcore "github.com/unofficialpku/disk/internal/mount"
	"github.com/unofficialpku/disk/internal/vfs"
)

type usageError struct {
	cause   error
	command string
}

func (e *usageError) Error() string {
	return fmt.Sprintf("%v\nRun '%s --help' for usage and examples.", e.cause, e.command)
}
func (e *usageError) Unwrap() error { return e.cause }
func usageFailure(cmd *cobra.Command, text string) error {
	return &usageError{errors.New(text), cmd.CommandPath()}
}
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var usage *usageError
	if errors.As(err, &usage) {
		return 2
	}
	return 1
}

func main() {
	desktopLaunch := desktop.StartedFromDesktop()
	args := launchArgs(os.Args[1:], desktopLaunch)
	// Our launch resolver replaces Cobra's Windows Explorer splash screen.
	cobra.MousetrapHelpText = ""
	guiLaunch := desktopLaunch && len(args) == 1 && args[0] == "gui"
	if guiLaunch {
		desktop.DetachConsole()
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := execute(ctx, args, os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "pkudisk:", err)
		os.Exit(exitCode(err))
	}
}
func execute(ctx context.Context, args []string, in io.Reader, out, errOut io.Writer) error {
	root := newRootCommand(in, out, errOut)
	root.InitDefaultHelpFlag()
	cmd, remaining, err := root.Find(args)
	// Validate command groups before Cobra handles --help, so unknown
	// subcommands cannot silently succeed by printing the parent's help.
	if err == nil && cmd.HasAvailableSubCommands() {
		cmd.InitDefaultHelpFlag()
		if err := cmd.ParseFlags(remaining); err != nil {
			return usageFailure(cmd, err.Error())
		}
		if err := cobra.NoArgs(cmd, cmd.Flags().Args()); err != nil {
			return usageFailure(cmd, err.Error())
		}
	}
	started := false
	root.PersistentPreRun = func(*cobra.Command, []string) { started = true }
	root.SetArgs(args)
	cmd, err = root.ExecuteContextC(ctx)
	if err != nil && !started {
		var usage *usageError
		if !errors.As(err, &usage) {
			if cmd == nil {
				cmd = root
			}
			return &usageError{err, cmd.CommandPath()}
		}
	}
	return err
}

const authHelp = `Account selection:
  Uses the saved default account. --account NAME selects another saved account.
  Explicit credentials start a new login. Successful logins are saved unless
  --no-save is set; the first saved account becomes the default.
  --non-interactive disables prompts; supply all required inputs explicitly.
  Run "pkudisk account add --help" for credential-file formats.

Output:
  Results go to stdout. Account announcements, errors, and transfer progress go
  to stderr. Exit codes: 0 success, 1 execution failure, 2 invalid command/flags.`
const remoteHelp = `Remote paths:
  Use "/Library/folder" paths, quoting spaces, or gns:// IDs returned by ls.
  Only accessible libraries are accepted. Run "pkudisk ls" to find names.`

func newRootCommand(in io.Reader, out, errOut io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use: "pkudisk", Short: "Browse, transfer, and mount PKUDisk files",
		Long: `Browse PKUDisk, transfer files, or mount it on Linux, macOS or Windows.

Start with "account add" to save a PKU campus login, then "ls" to list
accessible document libraries. "ls PATH" lists remote folders; "tui" opens a file
browser. "gui" opens the bilingual desktop tray and browser interface.
"search" finds remote filenames; "copy" duplicates remote files or
folders. "share" manages internal access, "link" manages anonymous links, and
"quota" reports capacity. "recover" inspects writes retained after a mount.
"delete" moves files to the recycle bin; "recycle" restores or permanently
removes deleted objects; "revision" inspects and restores file history.

Run "pkudisk help COMMAND" or "COMMAND --help" for usage and examples.
Flags may appear before or after arguments. Use -- before a file name that
starts with a dash. Exit codes: 0 success, 1 execution failure, 2 usage error.`,
		Example:      "  pkudisk account add\n  pkudisk ls\n  pkudisk ls '/My library/Reports'\n  pkudisk tui",
		SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error { return &usageError{err, cmd.CommandPath()} })
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddGroup(
		&cobra.Group{ID: "browse", Title: "Browse and find:"},
		&cobra.Group{ID: "files", Title: "Transfer and manage files:"},
		&cobra.Group{ID: "sharing", Title: "Sharing:"},
		&cobra.Group{ID: "local", Title: "Mount and recover:"},
		&cobra.Group{ID: "account", Title: "Accounts and capacity:"},
		&cobra.Group{ID: "tools", Title: "CLI tools:"},
	)
	root.SetHelpCommand(&cobra.Command{
		Use: "help [COMMAND...]", Short: "Explain a command, its arguments, and examples", GroupID: "tools",
		Long:    "Show help without opening credentials or connecting to PKUDisk.\nSpecify nested commands, such as 'help account add'.",
		Example: "  pkudisk help recover\n  pkudisk help account add",
		RunE: func(cmd *cobra.Command, args []string) error {
			target, remaining, err := cmd.Root().Find(args)
			if err != nil {
				return usageFailure(cmd, err.Error())
			}
			if len(remaining) > 0 {
				return usageFailure(cmd, "unknown help topic "+remaining[0])
			}
			target.InitDefaultHelpFlag()
			return target.Help()
		},
	})
	add := func(group string, commands ...*cobra.Command) {
		for _, command := range commands {
			command.GroupID = group
		}
		root.AddCommand(commands...)
	}
	add("browse", newRemoteCommand("ls"), newSearchCommand(), newRemoteCommand("tui"), newGUICommand())
	add("files", newRemoteCommand("upload"), newRemoteCommand("download"), newCopyCommand(), newRemoteCommand("extract"), newDeleteCommand(), newRecycleCommand(), newRevisionCommand())
	add("sharing", newShareCommand(), newLinkCommand())
	add("local", newRemoteCommand("mount"), newRecoverCommand())
	add("account", newAccountCommand(), newQuotaCommand())
	add("tools", newCompletionCommand())
	add("tools", &cobra.Command{Use: "version", Short: "Print the installed version", Args: cobra.NoArgs, Run: func(cmd *cobra.Command, _ []string) {
		fmt.Fprintln(cmd.OutOrStdout(), "pkudisk", installedVersion())
	}})
	return root
}

func newRemoteCommand(name string) *cobra.Command {
	var credentials authFlags
	var overwrite, jsonOutput, archive, viaArchive, fast bool
	var allAccounts bool
	var sizes sizeFormat
	timeout := 10 * time.Minute
	mount := mountcore.DefaultOptions()
	cmd := &cobra.Command{Use: name, Args: cobra.NoArgs}
	switch name {
	case "ls":
		cmd.Use = "ls [REMOTE-DIRECTORY]"
		cmd.Args = cobra.MaximumNArgs(1)
		cmd.Short = "List a remote directory or the accessible libraries"
		cmd.Long = `List files and folders in a remote directory. Omit the path, or use /,
to list accessible document libraries. Library rows show name, type,
human-readable USED, RW/RO access, and gns:// ID. Writable library usage
includes quota and percentage; read-only libraries show only used space.
Sizes use readable IEC units by default; -b/--bytes selects exact byte counts.
Subfolders show - for size.
Use --json for an array of entries with the original remote metadata.
A file path is rejected; use download to retrieve its contents.

` + remoteHelp
		cmd.Example = "  pkudisk ls\n  pkudisk ls --account work --json\n  pkudisk ls '/My library/Reports'\n  pkudisk ls --json 'gns://LIBRARY-ID/FOLDER-ID'"
	case "upload":
		cmd.Use = "upload LOCAL-PATH REMOTE-DESTINATION"
		cmd.Args = cobra.ExactArgs(2)
		cmd.Short = "Upload a local file or directory tree"
		cmd.Long = `Upload a regular file or recursively upload a directory, including empty
folders. Symlinks and special files are rejected. An existing remote directory
receives the local basename; a new destination path can rename the source.
The destination's parent must already exist.

--overwrite replaces files after checking their revisions and merges directory
trees, keeping unrelated entries. Files publish individually: an interrupted
tree transfer can leave completed files and folders at the destination.

Uploads send each file directly, using multipart uploads for large files with
no fixed client file-size cap. The server's archive extractor does not reliably
preserve nested/empty folders, so directory uploads use this direct path.

--fast first checks the first 200 KiB MD5 and file size. A possible match reads
the file once to calculate full MD5/CRC32 with bounded memory, then asks the
server to reuse existing content without sending file bytes. A non-match falls
back to ordinary upload. Hashing has its own progress phase. Existing files
always use revision-checked ordinary upload, including with --fast --overwrite.

Progress goes to stderr. --io-timeout bounds the entire operation, including
scanning and transferring the tree. Account quotas and file policies apply.

` + remoteHelp
		cmd.Example = "  pkudisk upload ./report.pdf '/My library/Reports/'\n  pkudisk upload ./Photos '/My library/'\n  pkudisk upload ./Photos '/My library/' --overwrite"
		cmd.Flags().BoolVarP(&overwrite, "overwrite", "f", false, "replace matching files and merge directories; keep unrelated entries")
		cmd.Flags().BoolVar(&fast, "fast", false, "try hash-based instant upload for new files; fall back on a non-match")
	case "download":
		cmd.Use = "download REMOTE-PATH [LOCAL-DESTINATION]"
		cmd.Args = cobra.RangeArgs(1, 2)
		cmd.Short = "Download a file, directory tree, or server-generated ZIP"
		cmd.Long = `Download a file or recursively download a directory, including empty folders.
An existing local directory receives the remote basename. Omit the destination
to use that name in the current directory. A new path can rename the result.
The local parent directory must already exist.

Directory modes (choose one):
  default        Transfer each file directly; no archive service required.
  --archive      Ask the server to package the folder and save NAME.zip.
  --via-archive  Download one server ZIP and extract it into a local folder.

Archive modes reduce per-file requests. The observed ZIP uses stored entries,
so payload bytes are not reduced. Server archive limits apply.
They need local space for the ZIP; --via-archive also needs space for extracted
files. ZIP paths are checked before extraction; symlinks and unsafe names are
rejected. Omit archive flags for direct transfers using bounded byte ranges
with no fixed client file-size cap. Archives keep the selected top-level folder.

--overwrite atomically replaces matching files or the ZIP and merges directory
trees, keeping unrelated entries. Failed file transfers preserve existing files.
Directory files install individually: cancellation can leave a partial tree.
Progress goes to stderr. --io-timeout bounds the entire operation.

` + remoteHelp
		cmd.Example = "  pkudisk download '/My library/report.pdf' ~/Downloads/\n  pkudisk download '/My library/Photos' ~/Downloads/\n  pkudisk download '/My library/Photos' ./photos.zip --archive\n  pkudisk download '/My library/Photos' ./photos --via-archive"
		cmd.Flags().BoolVarP(&overwrite, "overwrite", "f", false, "atomically replace matching files/ZIPs and merge directories; keep unrelated entries")
		cmd.Flags().BoolVar(&archive, "archive", false, "package a remote directory on the server and save it as a ZIP")
		cmd.Flags().BoolVar(&viaArchive, "via-archive", false, "download a directory as one server ZIP, then extract locally")
	case "extract":
		cmd.Use = "extract REMOTE-ARCHIVE REMOTE-DESTINATION"
		cmd.Args = cobra.ExactArgs(2)
		cmd.Short = "Extract an archive on the server into a new remote folder"
		cmd.Long = `Ask the netdisk to extract an existing remote archive without downloading it.
An existing destination directory receives a new folder named after the archive
without its suffix; a new path selects the folder name. The parent must exist.
Existing folders/files are never replaced. Formats and extraction size limits
are determined by the server. ZIP is verified on PKUDisk. The server can
omit empty directories and produce duplicate nested folder names. We check the
output tree for ambiguous paths and report an error if found. Use direct upload for a reliable local tree
transfer.

Shows server job progress and waits for completion. Errors can leave a partial
remote folder. Cancellation or --io-timeout stops waiting; the server job may
continue because the official protocol exposes no job cancellation endpoint.

` + remoteHelp
		cmd.Example = "  pkudisk extract '/My library/photos.zip' '/My library/'\n  pkudisk extract '/My library/photos.zip' '/My library/Photos-new'"
	case "tui":
		cmd.Short = "Open an interactive remote file browser"
		cmd.Long = "Browse remote libraries with a searchable list and use a local file picker\nfor uploads. Downloads use an editable destination. Transfers show progress\nand can be cancelled; a terminal is required.\n\nKeys: arrows/j/k move, Enter opens, Backspace returns to the parent, / filters,\nu uploads files/folders, d downloads files/folders, a saves a folder ZIP,\nv transfers a folder via ZIP, x extracts a remote archive, r refreshes,\ns opens sharing (internal links, recipients, external links and revocation),\nDelete deletes into the recycle bin, b opens the current/selected library's\nrecycle bin, H opens a file's version history, F toggles instant upload\n(off by default; new files only), ? shows help, Esc cancels, q quits.\nRecycle restoration, permanent removal/emptying and version restoration\nshow a confirmation. The screen shows the selected account and remote path."
		cmd.Example = "  pkudisk tui\n  pkudisk tui --account work --io-timeout 30m"
		cmd.Long += "\n\n--all-accounts shows saved accounts as top-level directories, followed by\neach account's libraries. Entering an account changes the displayed account\nand routes uploads, downloads, extraction and sharing to that session.\nAccount directories are virtual; open a library before file operations.\nAll saved accounts must authenticate and list their libraries at startup.\nThe saved default stays unchanged. --all-accounts cannot be combined with\n--account, --server, explicit credentials or --no-save."
		cmd.Example += "\n  pkudisk tui --all-accounts"
		cmd.Flags().BoolVar(&allAccounts, "all-accounts", false, "browse every saved account under its own top-level directory")
	case "mount":
		cmd.Use = "mount MOUNTPOINT"
		cmd.Args = cobra.ExactArgs(1)
		cmd.Short = "Mount remote libraries or a selected folder as a local filesystem"
		cmd.Long = `Mount PKUDisk as a filesystem. The process stays in the foreground.
Omit --root to expose all accessible libraries of the selected account.
--all-accounts mounts every saved account under its own first-level directory,
named after the saved account. It cannot be combined with --account, --root,
--server, explicit credentials or --no-save. All accounts must authenticate
and list their libraries at startup. Account directories are read-only; files
inside route to that account. Cross-account moves return EXDEV; copy instead.
Capacity counts shared libraries on the same server once, with available
space only where at least one account has write permission.

` + mountcore.PlatformHelp + `

Files up to --read-whole-file are fetched whole on first read. Larger files
start with --read-block-size ranges; sequential reads grow the fetch up to
--read-ahead. Seeks reset that growth. Overlapping reads share one fetch.
--read-concurrency bounds active fetches; cached reads do not queue behind them.
Reads return when their requested bytes arrive; the shared range keeps filling.
Only a complete, validated range enters the disk cache. Unmount stops active
fills. Size flags accept bytes or units such as 4MiB and 10GiB.
Diagnostic sizes use IEC units; -b/--bytes shows exact byte counts. Larger fetches
reduce round trips but use more bandwidth and memory. Set --read-whole-file 0
and --read-ahead 0 for small on-demand ranges.

Reads and writes share --cache-limit. Clean data is evicted first; pending
writes are protected. Whole-file reads require the file to fit in this budget.
Writes are uploaded on fsync/close. Interrupted writes may remain in the cache;
use recover to locate them. --read-only prevents remote file changes.
An expired session ends the mount.

--verbose explains operations, cache decisions, queue waits and HTTP request
phases on stderr. START and END lines share an operation number; a START
without END shows an outstanding operation. Logs include paths and file IDs,
but omit credentials, request bodies and signed URL paths/queries.
--debug additionally prints raw filesystem-driver diagnostics.

` + remoteHelp
		cmd.Example = mountcore.PlatformExample + "\n  pkudisk mount " + mountcore.PlatformPoint + " --all-accounts" + "\n  pkudisk mount " + mountcore.PlatformPoint + " --read-whole-file 32MiB --read-ahead 64MiB\n  pkudisk mount " + mountcore.PlatformPoint + " --read-whole-file 0 --read-block-size 1MiB --read-ahead 0" + "\n  pkudisk mount " + mountcore.PlatformPoint + " --verbose 2> mount.log"
		f := cmd.Flags()
		f.BoolVar(&allAccounts, "all-accounts", false, "mount every saved account in its own top-level directory; requires all accounts to be usable")
		f.StringVar(&mount.Root, "root", "", "remote library/folder path or gns:// ID to mount")
		f.StringVar(&mount.Cache, "cache-dir", defaultCache(), "private cache parent directory (0700); recover scans it")
		f.Var(mountcore.ByteSize{Value: &mount.CacheLimit}, "cache-limit", "shared read/write disk cache budget; clean data is evicted first; 0 disables the limit")
		f.Var(mountcore.ByteSize{Value: &mount.PartSize}, "part-size", "preferred upload chunk size, at least 5MiB; adjusted to server limits")
		f.Var(mountcore.ByteSize{Value: &mount.Read.BlockSize}, "read-block-size", "initial/random read range size (up to 512MiB)")
		f.Var(mountcore.ByteSize{Value: &mount.Read.WholeFile}, "read-whole-file", "fetch files up to this size whole; 0 disables; maximum 512MiB")
		f.Var(mountcore.ByteSize{Value: &mount.Read.ReadAhead}, "read-ahead", "maximum batched fetch for sequential reads; 0 disables growth; maximum 512MiB")
		f.IntVar(&mount.Read.Concurrency, "read-concurrency", mount.Read.Concurrency, "maximum simultaneous remote read fetches (1-64)")
		f.DurationVar(&mount.TTL, "cache-ttl", mount.TTL, "directory and attribute cache lifetime")
		f.DurationVar(&mount.RefreshInterval, "refresh-interval", mount.RefreshInterval, "renew idle sessions at this interval; 0 disables scheduled renewal")
		f.BoolVar(&mount.ReadOnly, "read-only", false, "prevent remote file changes")
		f.BoolVar(&mount.Debug, "debug", false, "print FUSE protocol diagnostics to stderr")
		f.BoolVarP(&mount.Verbose, "verbose", "v", false, "explain mount operations, cache/fetch decisions and HTTP timing on stderr; credentials and signed URLs are omitted")
	}
	credentials.add(cmd.Flags())
	if name == "ls" || name == "upload" || name == "download" || name == "mount" || name == "tui" {
		sizes.add(cmd)
		if name == "tui" {
			cmd.Flags().Lookup("bytes").Usage = "show exact byte counts for remote entries and transfer progress"
		}
	}
	if name == "ls" {
		cmd.Flags().BoolVarP(&jsonOutput, "json", "j", false, "write an array of entries as JSON to stdout")
	}
	if name == "upload" || name == "download" || name == "extract" || name == "tui" || name == "mount" {
		cmd.Flags().DurationVarP(&timeout, "io-timeout", "t", timeout, "timeout per operation (including a whole directory transfer); accepts 30s, 10m, 1h")
	}
	cmd.Long += "\n\n" + authHelp
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if err := credentials.validate(); err != nil {
			return usageFailure(cmd, err.Error())
		}
		if timeout <= 0 {
			return usageFailure(cmd, "--io-timeout must be positive")
		}
		if archive && viaArchive {
			return usageFailure(cmd, "choose --archive or --via-archive")
		}

		if allAccounts {
			for _, flag := range []string{"account", "root", "server", "username", "password-file", "credentials-file", "password-stdin", "no-save"} {
				if cmd.Flags().Changed(flag) {
					return usageFailure(cmd, "--all-accounts cannot be combined with --"+flag)
				}
			}
		}
		if name == "mount" {
			mount.Timeout = timeout
			if err := mount.Validate(); err != nil {
				return usageFailure(cmd, err.Error())
			}
		}
		if name == "tui" && !interactiveIO(cmd.InOrStdin(), cmd.OutOrStdout()) {
			return errors.New("tui requires an interactive terminal; use ls, upload, or download for non-interactive operations")
		}
		if name == "mount" {
			if err := mountcore.CheckSupport(); err != nil {
				return err
			}
		}
		credentials.input, credentials.output = cmd.InOrStdin(), cmd.ErrOrStderr()
		if name == "mount" && mount.Verbose {
			mount.Trace = diagnostic.New(cmd.ErrOrStderr())
			mount.Trace.SetBytes(sizes.bytes)
			credentials.trace = mount.Trace
			mount.Trace.Printf(cmd.Context(), "mount diagnostics enabled; version=%s OS=%s architecture=%s", installedVersion(), runtime.GOOS, runtime.GOARCH)
		}
		credentials.announce = name != "tui"
		if name == "mount" {
			mount.Point, mount.Timeout = args[0], timeout
			source, err := prepareAccounts(cmd.Context(), credentials, mountcore.SourceOptions{AllAccounts: allAccounts, Root: mount.Root, PartSize: mount.PartSize, Timeout: timeout, Trace: mount.Trace})
			if err != nil {
				return err
			}
			defer source.Close()
			return mountcore.Run(cmd.Context(), source, mount, cmd.ErrOrStderr())
		}
		if name == "tui" && allAccounts {
			source, err := prepareAccounts(cmd.Context(), credentials, mountcore.SourceOptions{AllAccounts: true, Timeout: timeout})
			if err != nil {
				return err
			}
			defer source.Close()
			return runTUI(cmd.Context(), source.Backend, "All accounts", fmt.Sprintf("%d saved accounts", source.AccountCount()), timeout, sizes, cmd.InOrStdin(), cmd.OutOrStdout())
		}
		session, server, account, err := authenticate(cmd.Context(), credentials)
		if err != nil {
			return err
		}
		defer session.Close()
		client, err := anyshare.New(server, session)
		if err != nil {
			return err
		}
		client.SetTrace(mount.Trace)
		switch name {
		case "ls":
			selector := "/"
			if len(args) == 1 {
				selector = args[0]
			}
			entries, err := listRemote(cmd.Context(), client, selector)
			if err != nil {
				return err
			}
			if !jsonOutput && (selector == "/" || selector == "") {
				return printLibraries(cmd.Context(), client, entries, sizes, cmd.OutOrStdout(), cmd.ErrOrStderr())
			}
			return printEntries(cmd.OutOrStdout(), entries, jsonOutput, sizes)
		case "upload", "download", "extract":
			destination := ""
			if len(args) == 2 {
				destination = args[1]
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			return runTransfer(ctx, client, name, args[0], destination, overwrite, archive, viaArchive, fast, sizes, cmd.OutOrStdout(), cmd.ErrOrStderr())
		case "tui":
			return runTUI(cmd.Context(), client, account, server, timeout, sizes, cmd.InOrStdin(), cmd.OutOrStdout())
		}
		return nil
	}
	return cmd
}

func defaultCache() string { return mountcore.DefaultCache() }
func listRemote(ctx context.Context, client *anyshare.Client, selector string) ([]anyshare.Entry, error) {
	if selector == "/" || selector == "" {
		return client.Roots(ctx)
	}
	entry, err := client.Resolve(ctx, selector)
	if err != nil {
		return nil, err
	}
	if !entry.Dir {
		return nil, fmt.Errorf("%q is a file; ls requires a directory (use download to retrieve it)", selector)
	}
	return client.List(ctx, entry.ID)
}
func printEntries(output io.Writer, entries []anyshare.Entry, asJSON bool, sizes sizeFormat) error {
	return display.RenderList(output, entries, display.ListOptions[anyshare.Entry]{
		JSON: asJSON, Indent: "  ", Headers: []string{"NAME", "TYPE", "SIZE", "ID"}, Numbers: []int{2},
		Row: func(entry anyshare.Entry) []string {
			kind := entry.Type
			if kind == "" {
				if entry.Dir {
					kind = "folder"
				} else {
					kind = "file"
				}
			}
			size := "-"
			if !entry.Dir && entry.Size >= 0 {
				size = sizes.value(entry.Size)
			}
			return []string{entry.Name, kind, size, entry.ID}
		},
	})
}
func newRecoverCommand() *cobra.Command {
	var cache string
	var jsonOutput bool
	var sizes sizeFormat
	cmd := &cobra.Command{Use: "recover", Short: "Inspect uncommitted local writes retained in the mount cache", Args: cobra.NoArgs,
		Long: `Find local cached file contents that have not been committed to PKUDisk.
This is useful after an upload failed, a write conflicted, or a mount ended
unexpectedly. Scans --cache-dir and its mount subdirectories.

This command lists retained files; it does not upload, delete, or restore them.
It requires no account or network connection. Stop the affected mount before
copying retained contents, as an active mount may still be writing them.

Each record shows a local data path, recorded remote name/ID, size, revision,
and update time. Multi-account mounts also record the owning account and server;
select that account with --account when uploading recovered contents.
Copy needed files to a permanent local directory, then use
upload to save them to a chosen remote destination. Prefer a new file name
when resolving a remote revision conflict. Remove old cache files only after
checking that your copies or uploads contain the expected data.

Output: human-readable records, or a JSON array with --json. Empty results are
reported explicitly; JSON uses []. Exit codes: 0 success, 1 scan failure,
2 invalid arguments. Sizes use IEC units by default; -b selects exact bytes.`,
		Example: "  pkudisk recover\n  pkudisk recover --cache-dir ~/.cache/pkudisk --json\n  cp /cache/mount-EXAMPLE/data-EXAMPLE ~/recovered-report.pdf\n  pkudisk upload ~/recovered-report.pdf '/My library/recovered-report.pdf'",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cache = localfile.ExpandPath(cache)
			records, err := vfs.RecoveryFiles(cache)
			if err != nil {
				// A missing cache parent is an empty result. A file disappearing
				// inside an existing cache is a scan failure, not a clean cache.
				if _, statErr := os.Stat(cache); !errors.Is(statErr, os.ErrNotExist) {
					return fmt.Errorf("scan cache %q: %w", cache, err)
				}
			}
			if records == nil {
				records = []vfs.RecoveryRecord{}
			}
			if jsonOutput {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(records)
			}
			if len(records) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "No uncommitted cached writes found in %s.\n", cache)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Found %d uncommitted cached file(s) in %s:\n", len(records), cache)
			for _, r := range records {
				fmt.Fprintf(cmd.OutOrStdout(), "\nLocal file: %s\nRemote:     %s (%s)\nSize:       %s\nRevision:   %s\nUpdated:    %s\n", r.Data, r.Entry.Name, r.Entry.ID, sizes.label(r.Entry.Size), r.Entry.Rev, r.Updated.Format(time.RFC3339))
				if r.Account != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "Account:    %s (%s)\n", r.Account, r.Server)
				}
			}
			fmt.Fprintln(cmd.OutOrStdout(), "\nStop the affected mount, copy needed local files, then upload to your chosen destination.\nFor a recovery example, run 'pkudisk recover --help'.")
			return nil
		},
	}
	sizes.add(cmd)
	cmd.Flags().StringVar(&cache, "cache-dir", defaultCache(), "mount cache parent or a specific mount cache directory to scan")
	cmd.Flags().BoolVarP(&jsonOutput, "json", "j", false, "output recovery records as a JSON array, including [] when empty")
	return cmd
}
func newCompletionCommand() *cobra.Command {
	return &cobra.Command{Use: "completion SHELL", Short: "Generate shell completion for bash, zsh, fish, or powershell", Args: cobra.ExactArgs(1),
		Long:      "Print a shell completion script to stdout. Source the script in your shell\nor install it in your shell's completion directory. No login is required.",
		Example:   "  source <(pkudisk completion bash)\n  pkudisk completion zsh > ~/.zsh/completions/_pkudisk\n  pkudisk completion fish > ~/.config/fish/completions/pkudisk.fish",
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletionV2(cmd.OutOrStdout(), true)
			case "zsh":
				return cmd.Root().GenZshCompletion(cmd.OutOrStdout())
			case "fish":
				return cmd.Root().GenFishCompletion(cmd.OutOrStdout(), true)
			case "powershell":
				return cmd.Root().GenPowerShellCompletionWithDesc(cmd.OutOrStdout())
			default:
				return usageFailure(cmd, "supported shells: bash, zsh, fish, powershell")
			}
		},
	}
}
