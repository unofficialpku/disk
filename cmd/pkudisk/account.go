package main

import (
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"
	"github.com/unofficialpku/disk/internal/accounts"
	"github.com/unofficialpku/disk/internal/display"
	"github.com/unofficialpku/disk/internal/localfile"
)

const accountStorageHelp = `Storage defaults:
  Linux:   ~/.config/pkudisk/accounts.json
  macOS:   ~/Library/Application Support/pkudisk/accounts.json
  Windows: %APPDATA%\pkudisk\accounts.json
XDG_CONFIG_HOME overrides the configuration parent on all platforms.
Unix uses file mode 0600 and directory mode 0700. Windows uses a private ACL
granting access only to the current user and SYSTEM.`

func newAccountCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "account", Short: "Save logins, list accounts, and choose the default",
		Long: `Manage PKU campus logins saved in the user configuration directory.
Accounts include a password and renewable session in a private local file.
The first account becomes the default. Remote commands use that default, or
--account NAME. Use 'account ls' to inspect saved accounts. No secrets are displayed.

Commands: add verifies and saves a login; ls displays saved accounts;
default selects one; remove deletes its saved credentials (not remote files).`,
		Example: "  pkudisk account add work\n  pkudisk account ls\n  pkudisk account default work\n  pkudisk ls --account work",
		RunE:    func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	var jsonOutput bool
	ls := &cobra.Command{Use: "ls", Short: "List saved accounts and mark the default", Args: cobra.NoArgs,
		Long:    "List saved account names, usernames, servers, and the default marker.\nPasswords and tokens are excluded from both text and JSON. No network access\nor login is needed.\n\n" + accountStorageHelp,
		Example: "  pkudisk account ls\n  pkudisk account ls --json",
		RunE:    func(cmd *cobra.Command, _ []string) error { return lsAccounts(cmd.OutOrStdout(), jsonOutput) },
	}
	ls.Flags().BoolVarP(&jsonOutput, "json", "j", false, "output account summaries as a JSON array without passwords or tokens")
	var opts authFlags
	add := &cobra.Command{Use: "add [NAME]", Short: "Verify a campus login and save it locally", Args: cobra.MaximumNArgs(1),
		Long: `Log in through PKU unified authentication and save the username, password,
and renewable session. NAME is a local label (default: campus username).
The first saved account is the default; --default switches it explicitly.
--replace updates an existing label only
after a successful login. CAPTCHA or OTP/SMS may require interaction.

Credentials:
  With no flags, prompt for a username and hidden password.
  --username ID --password-file FILE reads a regular owner-only password file.
  --password-stdin reads a password from stdin, with an optional final newline.
  --credentials-file FILE reads owner-only JSON, for example:
    {"username":"YOUR-ID","password":"YOUR-PASSWORD","otp":"123456"}
  otp and sms are optional, transient verification codes; they are not saved.
  JSON credentials cannot be combined with other credential flags.
  --non-interactive fails if credentials or required verification are missing.

Results/status go to stderr. Exit codes: 0 success, 1 login/save failure,
2 invalid arguments.` + "\n\n" + accountStorageHelp + "\nCredential files must use " + localfile.PermissionsHelp + ".",
		Example: "  pkudisk account add work\n  pkudisk account add work --username YOUR-ID --password-file ~/.config/pkudisk/password\n  pkudisk account add work --replace --default",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := opts.validate(); err != nil {
				return usageFailure(cmd, err.Error())
			}
			if len(args) == 1 {
				opts.saveName = args[0]
			}
			if opts.saveName != "" && !accounts.ValidName(opts.saveName) {
				return usageFailure(cmd, "invalid account name; use a label without slashes, leading/trailing spaces, or control characters")
			}
			opts.forceLogin = true
			opts.input = cmd.InOrStdin()
			opts.output = cmd.ErrOrStderr()
			s, _, _, err := authenticate(cmd.Context(), opts)
			if s != nil {
				s.Close()
			}
			return err
		},
	}
	opts.addCredentials(add.Flags())
	add.Flags().BoolVar(&opts.makeDefault, "default", false, "select this account as the default after login")
	add.Flags().BoolVar(&opts.replace, "replace", false, "update an existing account label after successful login")
	setDefault := &cobra.Command{Use: "default NAME", Short: "Choose the account used by remote commands", Args: cobra.ExactArgs(1),
		Long:    "Select a saved account as the default for mount, ls, upload, download,\nand tui. Does not log in or change any remote file. NAME must appear in\n'account ls'.",
		Example: "  pkudisk account default work",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := accounts.Default()
			if err != nil {
				return err
			}
			if err = s.SetDefault(args[0]); err != nil {
				return err
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "Default account:", args[0])
			return nil
		},
	}
	remove := &cobra.Command{Use: "remove NAME", Short: "Delete a saved login from this computer", Args: cobra.ExactArgs(1),
		Long:    "Delete an account's locally saved password and session. Remote files and\nother saved accounts are unaffected. If removing the default, the first\nremaining account becomes the default. No server request is made.",
		Example: "  pkudisk account remove work",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := accounts.Default()
			if err != nil {
				return err
			}
			if err = s.Remove(args[0]); err != nil {
				return err
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "Removed account:", args[0])
			return nil
		},
	}
	cmd.AddCommand(add, ls, setDefault, remove)
	return cmd
}
func lsAccounts(output io.Writer, jsonOutput bool) error {
	store, err := accounts.Default()
	if err != nil {
		return err
	}
	cfg, err := store.Load()
	if err != nil {
		return err
	}
	type summary struct {
		Name     string `json:"name"`
		Username string `json:"username"`
		Server   string `json:"server"`
		Default  bool   `json:"default"`
	}
	rows := make([]summary, 0, len(cfg.Accounts))
	for _, a := range cfg.Accounts {
		rows = append(rows, summary{a.Name, a.Username, a.Server, a.Name == cfg.Default})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return display.RenderList(output, rows, display.ListOptions[summary]{
		JSON: jsonOutput, Indent: "  ", Headers: []string{"DEFAULT", "NAME", "USERNAME", "SERVER"},
		EmptyMessage: "No saved accounts. Run 'pkudisk account add' to log in and save the first account.",
		Row: func(row summary) []string {
			marker := ""
			if row.Default {
				marker = "*"
			}
			return []string{marker, row.Name, row.Username, row.Server}
		},
	})
}
