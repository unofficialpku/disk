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

const shareHelp = `Internal links require login and honor existing permissions. Creating a link
alone grants no access. Add/remove changes direct user or department rules;
inherited access remains. Recipient IDs are AnyShare IDs, not campus login
names. Find them with 'share search NAME'. No notifications are sent.

Approval requests are reported as pending errors, never as effective grants.
Removal uses the official full ACL update, preserving unrelated rules and
inheritance. The server exposes no conditional update; avoid editing the same
ACL concurrently in another client.

Missing required input is prompted in an interactive terminal. --non-interactive
disables all prompts. Scripts must supply required parameters.`

const linkHelp = `External links allow access without an AnyShare account. Server defaults
control permissions, expiry and visit limits unless flags override them.
Pickup codes and phone verification can be required by server policy.
Creation sends no notifications. Revoking a link leaves internal permissions.

Missing required input is prompted in an interactive terminal. --non-interactive
disables all prompts. Scripts must supply required parameters.`

func newShareCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "share", Short: "Create internal links and manage user/department access", Long: shareHelp,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	for _, action := range []string{"link", "add", "ls", "remove", "search"} {
		cmd.AddCommand(newSharingCommand(false, action))
	}
	return cmd
}

func newLinkCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "link", Short: "Create, inspect and revoke external anonymous links", Long: linkHelp,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() }}
	for _, action := range []string{"add", "ls", "remove"} {
		cmd.AddCommand(newSharingCommand(true, action))
	}
	return cmd
}

type sharingFlags struct {
	auth        commandAuth
	json        bool
	users, deps []string
	options     anyshare.ShareOptions
	expires     time.Duration
	limit       int64
	searchLimit int
}

func (f *sharingFlags) recipients() []anyshare.Recipient {
	result := []anyshare.Recipient{}
	for _, id := range f.users {
		result = append(result, anyshare.Recipient{ID: id, Type: "user"})
	}
	for _, id := range f.deps {
		result = append(result, anyshare.Recipient{ID: id, Type: "department"})
	}
	return result
}

func newSharingCommand(external bool, action string) *cobra.Command {
	var flags sharingFlags
	group, argument := "share", "REMOTE-PATH"
	if external {
		group = "link"
	}
	if action == "search" {
		argument = "NAME"
	} else if external && action == "remove" {
		argument = "LINK-ID"
	}
	cmd := &cobra.Command{Use: action + " [" + argument + "]", Args: cobra.MaximumNArgs(1)}
	switch action {
	case "link":
		cmd.Short = "Get or create an internal link without changing access"
		cmd.Long = "Return an existing authenticated navigation link, or create one.\nRecipients still need file permissions. The protocol exposes no operation\nto delete this link; revoke access with 'share remove'."
	case "add":
		cmd.Short = "Grant recipient access and return an internal link"
		cmd.Long = "Grant --user/--department IDs access to a remote file or folder, then return\nan internal link. Without recipients, a terminal prompts for a name search\nand selection. Permissions/validity use server defaults unless overridden.\nLink creation is separate: if it fails after a grant, permissions remain and\n'share link PATH' can be retried. Use share link for a link-only operation."
		if external {
			cmd.Short = "Create an external link for a file or folder"
			cmd.Long = "Create an anonymous link using server defaults for permissions, validity\nand visits. --expires-in 0 requests no expiry, subject to policy.\ncreate,modify permissions enable folder uploads. A missing server-required\npickup code is prompted in a terminal. Results include effective settings."
		}
	case "ls":
		cmd.Short = "Show direct/inherited access and an existing internal link"
		cmd.Long = "Show the selected file/folder's permissions, including inherited rules.\nShows an existing internal link; this command creates nothing."
		if external {
			cmd.Short = "List all external links for a remote file or folder"
			cmd.Long = "List all pages of this object's external links, including IDs, permissions,\nexpiry, visit limits, pickup codes and URLs. This command creates nothing."
		}
	case "remove":
		cmd.Short = "Remove selected recipients' direct permission rules"
		cmd.Long = "Remove direct rules for --user/--department IDs, retaining unrelated and\ninherited rules. Missing direct rules fail before any change. Without IDs,\na terminal offers a choice from this object's direct user/department rules.\nRemoving rules does not delete the navigation link or inherited access."
		if external {
			cmd.Short = "Revoke one external link by its ID"
			cmd.Long = "Delete a link ID shown by 'link ls PATH'. IDs, rather than URLs,\nidentify the link. The file and internal permissions remain."
		}
	case "search":
		cmd.Short = "Find user/department IDs for sharing"
		cmd.Long = "Search one bounded directory page by name/account. Displayed IDs are used\nby --user/--department. Campus IAAA login numbers may differ from AnyShare\naccounts and need not match. No permissions are changed or messages sent."
	}
	if external {
		cmd.Long += "\n\n" + linkHelp
	} else {
		cmd.Long += "\n\n" + shareHelp
	}
	if action != "search" && !(external && action == "remove") {
		cmd.Long += "\n\n" + remoteHelp
	}
	cmd.Example = "  pkudisk " + group + " " + action + " '/My library/report.pdf'"
	if action == "search" {
		cmd.Example = "  pkudisk share search 'Recipient name'"
	} else if external && action == "remove" {
		cmd.Example = "  pkudisk link remove LINK-ID"
	} else if !external && (action == "add" || action == "remove") {
		cmd.Example += " --user USER-ID\n  pkudisk share " + action + " '/My library/Reports' --department DEPARTMENT-ID"
	} else if external && action == "add" {
		cmd.Example += " --expires-in 168h --code Abcd1234\n  pkudisk link add '/My library/Inbox' --permission create,modify --limit 10"
	}
	flags.auth.add(cmd)
	if action != "remove" {
		cmd.Flags().BoolVarP(&flags.json, "json", "j", false, "write structured sharing results as JSON")
	}
	if !external && (action == "add" || action == "remove") {
		cmd.Flags().StringSliceVar(&flags.users, "user", nil, "AnyShare user IDs; repeat or separate with commas; find with 'share search NAME'")
		cmd.Flags().StringSliceVar(&flags.deps, "department", nil, "department IDs; repeat or separate with commas; find with 'share search NAME'")
	}
	if action == "add" {
		cmd.Flags().StringSliceVar(&flags.options.Permissions, "permission", nil, "comma-separated preview,download,create,modify,delete,cache; defaults from server; display is included")
		cmd.Flags().DurationVar(&flags.expires, "expires-in", 0, "validity, e.g. 24h or 168h; 0 requests no expiry; omitted uses server default")
		if external {
			cmd.Flags().StringVar(&flags.options.Code, "code", "", "pickup code (4-100 ASCII letters/digits/~!%#$@-_.); prompted if required")
			cmd.Flags().StringVar(&flags.options.Title, "title", "", "link title (default: remote basename)")
			cmd.Flags().Int64Var(&flags.limit, "limit", -1, "maximum visits; -1 is unlimited; omitted uses server default")
			cmd.Flags().BoolVar(&flags.options.VerifyMobile, "verify-mobile", false, "require phone verification if allowed; server-forced verification always applies")
		}
	}
	if action == "search" {
		cmd.Flags().IntVar(&flags.searchLimit, "limit", 50, "maximum directory results (1-1000)")
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		return runSharingCommand(cmd, args, &flags, external, action, argument)
	}
	return cmd
}

func runSharingCommand(cmd *cobra.Command, args []string, flags *sharingFlags, external bool, action, argument string) error {
	if err := flags.auth.credentials.validate(); err != nil {
		return usageFailure(cmd, err.Error())
	}
	if flags.auth.timeout <= 0 {
		return usageFailure(cmd, "--io-timeout must be positive")
	}
	if cmd.Flags().Changed("expires-in") {
		flags.options.ExpiresIn = &flags.expires
	}
	if external && action == "add" && cmd.Flags().Changed("limit") {
		flags.options.Limit = &flags.limit
	}
	if err := flags.options.Validate(); err != nil {
		return usageFailure(cmd, err.Error())
	}
	recipients := flags.recipients()
	if err := anyshare.ValidateRecipients(recipients); err != nil {
		return usageFailure(cmd, err.Error())
	}
	canPrompt := !flags.auth.credentials.nonInteractive && interactiveIO(cmd.InOrStdin(), cmd.ErrOrStderr())
	selector := ""
	if len(args) == 1 {
		selector = args[0]
	} else {
		if !canPrompt {
			return usageFailure(cmd, "missing "+argument+"; supply it or run in a terminal without --non-interactive")
		}
		value, err := ttyInput(cmd.Context(), argument+": ", false)
		if err != nil {
			return err
		}
		selector = string(value)
	}
	if strings.TrimSpace(selector) == "" {
		return usageFailure(cmd, argument+" cannot be empty")
	}
	if !external && (action == "add" || action == "remove") && len(recipients) == 0 && !canPrompt {
		return usageFailure(cmd, "select --user ID or --department ID; find IDs with 'share search NAME'")
	}
	if action == "search" && (flags.searchLimit < 1 || flags.searchLimit > 1000 || len(selector) > 1024) {
		return usageFailure(cmd, "search needs a keyword up to 1024 bytes and --limit between 1 and 1000")
	}
	flags.auth.credentials.nonInteractive = !canPrompt
	client, closer, err := flags.auth.open(cmd)
	if err != nil {
		return err
	}
	defer closer.Close()
	ctx, cancel := context.WithTimeout(cmd.Context(), flags.auth.timeout)
	defer cancel()
	if action == "search" {
		found, err := client.SearchRecipients(ctx, selector, flags.searchLimit)
		if err != nil {
			return err
		}
		return printRecipients(cmd.OutOrStdout(), found, flags.json)
	}
	if external && action == "remove" {
		if err := client.RemoveExternalLink(ctx, selector); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Revoked external link", selector)
		return nil
	}
	entry, err := client.Resolve(ctx, selector)
	if err != nil {
		return err
	}
	if !external && (action == "add" || action == "remove") && len(recipients) == 0 {
		var candidates []anyshare.Recipient
		if action == "add" {
			query, err := ttyInput(ctx, "Recipient name/account to search: ", false)
			if err != nil {
				return err
			}
			candidates, err = client.SearchRecipients(ctx, string(query), 50)
			if err != nil {
				return err
			}
		} else {
			acl, err := client.SharePermissions(ctx, entry.ID)
			if err != nil {
				return err
			}
			for _, permission := range acl.Permissions {
				if permission.InheritedFrom == "" && (permission.Type == "user" || permission.Type == "department") {
					candidates = append(candidates, anyshare.Recipient{ID: permission.ID, Type: permission.Type, Name: permission.Name})
				}
			}
		}
		recipient, err := pickRecipient(ctx, candidates)
		if err != nil {
			return err
		}
		recipients = []anyshare.Recipient{recipient}
	}
	if action == "remove" {
		n, err := client.RevokeShare(ctx, entry.ID, recipients)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Removed %d direct permission rule(s). Inherited access remains in effect.\n", n)
		return nil
	}
	if action == "ls" {
		if external {
			links, err := client.ShareLinks(ctx, entry, true)
			if err != nil {
				return err
			}
			return printShareLinks(cmd.OutOrStdout(), links, flags.json)
		}
		acl, err := client.SharePermissions(ctx, entry.ID)
		if err != nil {
			return err
		}
		links, err := client.ShareLinks(ctx, entry, false)
		if err != nil {
			return err
		}
		return printShareACL(cmd.OutOrStdout(), acl, links, flags.json)
	}
	var link anyshare.ShareLink
	if external {
		link, err = client.CreateExternalLink(ctx, entry, flags.options)
		if errors.Is(err, anyshare.ErrPickupCodeRequired) && canPrompt {
			code, promptErr := ttyInput(ctx, "Pickup code (4-100 characters): ", true)
			if promptErr != nil {
				return promptErr
			}
			flags.options.Code = string(code)
			clear(code)
			link, err = client.CreateExternalLink(ctx, entry, flags.options)
		}
	} else {
		if action == "add" {
			if err := client.GrantShare(ctx, entry, recipients, flags.options); err != nil {
				return err
			}
		}
		link, err = client.InternalLink(ctx, entry)
		if err != nil && action == "add" {
			return fmt.Errorf("permissions granted; internal link failed (retry 'share link PATH'): %w", err)
		}
	}
	if err != nil {
		return err
	}
	return printShareLinks(cmd.OutOrStdout(), []anyshare.ShareLink{link}, flags.json)
}

func expiration(stamp *time.Time) string {
	if stamp == nil {
		return "never"
	}
	return stamp.Format(time.RFC3339)
}

func printShareLinks(out io.Writer, links []anyshare.ShareLink, asJSON bool) error {
	if !asJSON {
		for _, link := range links {
			if link.Type == "realname" {
				if _, err := fmt.Fprintln(out, display.Text(link.URL)); err != nil {
					return err
				}
			}
		}
	}
	return display.RenderList(out, links, display.ListOptions[anyshare.ShareLink]{
		JSON: asJSON, Headers: []string{"ID", "TITLE", "ALLOW", "EXPIRES", "VISITS", "CODE", "PHONE", "URL"},
		Row: func(link anyshare.ShareLink) []string {
			if link.Type == "realname" {
				return nil
			}
			limit, code := "unlimited", link.Code
			if link.LimitedTimes != -1 {
				limit = fmt.Sprint(link.LimitedTimes)
			}
			if code == "" {
				code = "-"
			}
			return []string{link.ID, link.Title, strings.Join(link.Permissions, ","), expiration(link.ExpiresAt), limit, code, fmt.Sprint(link.VerifyMobile), link.URL}
		},
	})
}

func printShareACL(out io.Writer, acl anyshare.ShareACL, links []anyshare.ShareLink, asJSON bool) error {
	if err := display.RenderList(out, acl.Permissions, display.ListOptions[anyshare.SharePermission]{
		JSON: asJSON, Headers: []string{"TYPE", "NAME", "ALLOW", "DENY", "EXPIRES", "SOURCE", "ID"},
		JSONValue: func(permissions []anyshare.SharePermission) any {
			acl.Permissions = permissions
			return struct {
				anyshare.ShareACL
				Links []anyshare.ShareLink `json:"links"`
			}{acl, append([]anyshare.ShareLink{}, links...)}
		},
		Row: func(permission anyshare.SharePermission) []string {
			from := permission.InheritedPath
			if from == "" {
				from = permission.InheritedFrom
			}
			if from == "" {
				from = "direct"
			}
			return []string{permission.Type, permission.Name, strings.Join(permission.Allow, ","), strings.Join(permission.Deny, ","), expiration(permission.ExpiresAt), from, permission.ID}
		},
	}); err != nil {
		return err
	}
	if !asJSON {
		for _, link := range links {
			if _, err := fmt.Fprintln(out, "Internal link:", display.Text(link.URL)); err != nil {
				return err
			}
		}
	}
	return nil
}

func printRecipients(out io.Writer, recipients []anyshare.Recipient, asJSON bool) error {
	return display.RenderList(out, recipients, display.ListOptions[anyshare.Recipient]{
		JSON: asJSON, Headers: []string{"TYPE", "NAME", "ACCOUNT", "PATH", "ID"},
		Row: func(recipient anyshare.Recipient) []string {
			return []string{recipient.Type, recipient.Name, recipient.Account, recipient.Path, recipient.ID}
		},
	})
}
