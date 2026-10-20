package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/unofficialpku/disk/internal/anyshare"
	"github.com/unofficialpku/disk/internal/auth"
	"github.com/unofficialpku/disk/internal/display"
)

type commandAuth struct {
	credentials authFlags
	timeout     time.Duration
}

func (o *commandAuth) add(cmd *cobra.Command) {
	o.timeout = 10 * time.Minute
	o.credentials.add(cmd.Flags())
	cmd.Flags().DurationVarP(&o.timeout, "io-timeout", "t", o.timeout, "timeout for the server operation; accepts 30s, 10m, 1h")
	cmd.Long += "\n\n" + authHelp
}

func (o *commandAuth) open(cmd *cobra.Command) (*anyshare.Client, *auth.Session, error) {
	if err := o.credentials.validate(); err != nil {
		return nil, nil, usageFailure(cmd, err.Error())
	}
	if o.timeout <= 0 {
		return nil, nil, usageFailure(cmd, "--io-timeout must be positive")
	}
	o.credentials.input, o.credentials.output = cmd.InOrStdin(), cmd.ErrOrStderr()
	o.credentials.announce = true
	session, server, _, err := authenticate(cmd.Context(), o.credentials)
	if err != nil {
		return nil, nil, err
	}
	client, err := anyshare.New(server, session)
	if err != nil {
		session.Close()
		return nil, nil, err
	}
	return client, session, nil
}

func newQuotaCommand() *cobra.Command {
	var auth commandAuth
	var sizes sizeFormat
	cmd := &cobra.Command{
		Use:   "quota",
		Short: "Show library allocation, usage and writable remaining capacity",
		Long: `Sum the accessible libraries' quota and used space. Writable libraries
contribute their allocation and usage; read-only libraries contribute used/used.
Available is the sum of writable libraries' remaining capacity. Personal and
shared custom libraries have independent quotas. The local cache is excluded.
This is the same total exposed by a default writable mount. Quota and permission
queries are read-only and do not enumerate file contents.`,
		Example: "  pkudisk quota\n  pkudisk quota --account work",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, closer, err := auth.open(cmd)
			if err != nil {
				return err
			}
			defer closer.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), auth.timeout)
			defer cancel()
			quota, err := client.Quota(ctx)
			if err != nil {
				return fmt.Errorf("read quota: %w", err)
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Allocated: %s\n", sizes.value(quota.Allocated))
			fmt.Fprintf(out, "Used:      %s\n", sizes.value(quota.Used))
			fmt.Fprintf(out, "Available: %s\n", sizes.value(quota.Available()))
			return nil
		},
	}
	auth.add(cmd)
	sizes.add(cmd)
	return cmd
}

func newSearchCommand() *cobra.Command {
	var auth commandAuth
	var scope string
	var limit int
	var jsonOutput bool
	var sizes sizeFormat
	cmd := &cobra.Command{
		Use:     "search KEYWORD",
		Short:   "Search remote filenames",
		Args:    cobra.ExactArgs(1),
		Example: "  pkudisk search report\n  pkudisk search report --in '/Library/Reports' --limit 100\n  pkudisk search report --json",
		Long: `Search file and folder names through AnyShare's server index.
The default searches all accessible libraries. --in resolves a remote folder
first and restricts the server query to that folder's descendants. Results are
one bounded page; --limit accepts 1 through 1000. Content and paragraph search
are not requested. A search never changes remote files.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if limit < 1 || limit > 1000 || strings.TrimSpace(args[0]) == "" || len(args[0]) > 1024 {
				return usageFailure(cmd, "search needs a nonempty keyword up to 1024 bytes and --limit between 1 and 1000")
			}
			client, closer, err := auth.open(cmd)
			if err != nil {
				return err
			}
			defer closer.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), auth.timeout)
			defer cancel()
			selector := ""
			if scope != "" {
				entry, err := client.Resolve(ctx, scope)
				if err != nil {
					return fmt.Errorf("resolve search scope: %w", err)
				}
				if !entry.Dir {
					return errors.New("search scope must be a remote folder")
				}
				selector = entry.ID
			}
			result, err := client.Search(ctx, args[0], selector, limit)
			if err != nil {
				return fmt.Errorf("search remote names: %w", err)
			}
			if err := display.RenderList(cmd.OutOrStdout(), result.Files, display.ListOptions[anyshare.SearchEntry]{
				JSON: jsonOutput, Headers: []string{"NAME", "SIZE", "PATH", "ID"}, Numbers: []int{1},
				JSONValue: func(files []anyshare.SearchEntry) any {
					return struct {
						Hits  int                    `json:"hits"`
						Next  int                    `json:"next"`
						Files []anyshare.SearchEntry `json:"files"`
					}{result.Hits, result.Next, files}
				},
				Row: func(file anyshare.SearchEntry) []string {
					size := "-"
					if file.Size >= 0 {
						size = sizes.value(file.Size)
					}
					return []string{file.Name, size, file.Path, file.ID}
				},
			}); err != nil {
				return err
			}
			if !jsonOutput {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Matches: %d; returned: %d; next: %d\n", result.Hits, len(result.Files), result.Next)
			}
			return err
		},
	}
	auth.add(cmd)
	sizes.add(cmd)
	cmd.Flags().StringVar(&scope, "in", "", "restrict the search to a remote folder path or gns:// ID")
	cmd.Flags().IntVar(&limit, "limit", 25, "maximum results to request (1-1000)")
	cmd.Flags().BoolVarP(&jsonOutput, "json", "j", false, "write matches and pagination fields as JSON")
	return cmd
}

func newCopyCommand() *cobra.Command {
	var auth commandAuth
	cmd := &cobra.Command{
		Use:   "copy REMOTE-SOURCE REMOTE-DIRECTORY",
		Short: "Copy a remote file or folder on the server",
		Args:  cobra.ExactArgs(2),
		Long: `Copy a remote file or directory directly on AnyShare. The destination
must be an existing remote folder; the source keeps its current name. File
copies return after the server publishes the new object. Directory copies may
run asynchronously and are polled until completion. No file bytes pass through
this client. Existing-name behavior follows the official client's server
policy; inspect the destination with ls after completion.`,
		Example: "  pkudisk copy '/Library/report.pdf' '/Library/Archive'\n  pkudisk copy '/Library/Photos' '/Library/Backup'",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, closer, err := auth.open(cmd)
			if err != nil {
				return err
			}
			defer closer.Close()
			ctx, cancel := context.WithTimeout(cmd.Context(), auth.timeout)
			defer cancel()
			source, err := client.Resolve(ctx, args[0])
			if err != nil {
				return fmt.Errorf("resolve copy source: %w", err)
			}
			destination, err := client.Resolve(ctx, args[1])
			if err != nil {
				return fmt.Errorf("resolve copy destination: %w", err)
			}
			if !destination.Dir {
				return errors.New("copy destination must be a remote folder")
			}
			result, err := client.Copy(ctx, source, destination, func(progress anyshare.CopyProgress) {
				if source.Dir && progress.Status != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "Copying %s: %d%% (%s)\n", source.Name, progress.Percent, progress.Status)
				}
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Copied %s -> %s/%s\n", source.ID, destination.ID, result.Name)
			return nil
		},
	}
	auth.add(cmd)
	return cmd
}
