package cli

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/dps/berth/internal/daemon"
	"github.com/dps/berth/internal/state"
)

// ---- sign-in (design.md, Sign-in) ----

func cmdAccess() *cobra.Command {
	var remove bool
	c := &cobra.Command{
		Use:   "access <app> [path] [public|signed-in|allow <who...>|off]",
		Short: "Google sign-in for an app or a path (show the policy with no level)",
		Example: "  berth access chirp public                  # open, but the app sees who is signed in\n" +
			"  berth access chirp signed-in               # any Google account\n" +
			"  berth access chirp allow you@gmail.com @family @example.com\n" +
			"  berth access chirp /admin/* allow @family  # path rule (trailing * = prefix)\n" +
			"  berth access chirp /admin/* --remove\n" +
			"  berth access chirp off",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, err := appArg(args)
			if err != nil {
				return err
			}
			rest := args[1:]
			p := daemon.AccessParams{App: app, Remove: remove}
			if len(rest) > 0 && strings.HasPrefix(rest[0], "/") {
				p.Path, rest = rest[0], rest[1:]
			}
			if len(rest) == 0 && !remove {
				if p.Path != "" {
					return fmt.Errorf("give a level for %s (public, signed-in, allow <who...>) or --remove", p.Path)
				}
				return showAccess(app)
			}
			if remove {
				if p.Path == "" || len(rest) > 0 {
					return fmt.Errorf("usage: berth access <app> <path> --remove")
				}
				return call("access.set", p, nil)
			}
			if rest[0] == "off" {
				if p.Path != "" || len(rest) > 1 {
					return fmt.Errorf("usage: berth access <app> off   (for a path rule, use --remove)")
				}
				p.Off = true
			} else {
				p.Level, p.Allow = rest[0], rest[1:]
			}
			return call("access.set", p, nil)
		},
	}
	c.Flags().BoolVar(&remove, "remove", false, "remove the rule for <path>")
	return c
}

func showAccess(app string) error {
	a, err := store().App(app)
	if err != nil {
		return err
	}
	if jsonOut {
		printJSON(map[string]any{"app": app, "access": a.Access})
		return nil
	}
	if a.Access == nil {
		fmt.Printf("%s has no sign-in policy (berth access %s public|signed-in|allow ...)\n", app, app)
		return nil
	}
	fmt.Printf("%-12s %s\n", "default", accessRule(a.Access.Level, a.Access.Allow))
	for _, r := range a.Access.Paths {
		fmt.Printf("%-12s %s\n", r.Path, accessRule(r.Level, r.Allow))
	}
	return nil
}

func accessRule(level string, allow []string) string {
	if level == state.LevelAllow {
		return level + " " + strings.Join(allow, " ")
	}
	return level
}

func cmdAuthSetup() *cobra.Command {
	var clientID, secret, host, owner string
	var noBrowser bool
	c := &cobra.Command{
		Use:   "auth:setup google",
		Short: "Set up Google sign-in (one OAuth client for every app)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "google" {
				return fmt.Errorf("only google is supported for now")
			}
			cfg, err := store().Config()
			if err != nil {
				return err
			}
			if cfg.Domain == "" || cfg.NoTunnel {
				return fmt.Errorf("sign-in needs the Cloudflare tunnel and a domain (Google only redirects to public hostnames)")
			}
			var checked map[string]string
			if err := call("auth.setup", daemon.AuthSetupParams{Provider: "google", Host: host, CheckOnly: true}, &checked); err != nil {
				return err
			}
			host = checked["host"]
			if clientID == "" || secret == "" {
				const console = "https://console.cloud.google.com/auth/overview"
				fmt.Printf(`Google doesn't let programs create OAuth clients, so this takes a minute in
the Google Cloud console (once; every app shares the client):

  1. Open %s
     and pick or create a project. If asked, "Get started": app name (say
     "Berth"), your email as support contact, audience External.
  2. Audience → Publish app. Berth asks only for name, email and photo, which
     needs no review by Google; until published, only test users can sign in.
  3. Clients → Create client → Web application. Under "Authorized redirect
     URIs" add exactly:

       https://%s/callback

  4. Create, then copy the Client ID and Client secret here.

`, console, host)
				if !noBrowser {
					exec.Command("open", console).Run()
				}
			}
			if clientID == "" {
				clientID = ask("Client ID: ")
			}
			if secret == "" {
				fmt.Print("Client secret (hidden): ")
				if term.IsTerminal(int(os.Stdin.Fd())) {
					b, err := term.ReadPassword(int(os.Stdin.Fd()))
					fmt.Println()
					if err != nil {
						return err
					}
					secret = string(b)
				} else {
					secret = ask("")
				}
			}
			if owner == "" {
				if ac, err := store().AuthConfig(); err == nil && len(ac.Groups["owners"]) == 0 {
					owner = ask("Your Google email (dev versions are open to the @owners group): ")
				}
			}
			if err := call("auth.setup", daemon.AuthSetupParams{Provider: "google", Host: host, ClientID: clientID, ClientSecret: secret, Owner: owner}, nil); err != nil {
				return err
			}
			if !jsonOut {
				fmt.Printf("\nTry it: open https://%s and sign in. Then put an app behind it:\n  berth access <app> signed-in\n", host)
			}
			return nil
		},
	}
	c.Flags().StringVar(&clientID, "client-id", "", "OAuth client ID")
	c.Flags().StringVar(&secret, "client-secret", "", "OAuth client secret (prompted if omitted)")
	c.Flags().BoolVar(&noBrowser, "no-browser", false, "don't open the Google Cloud console")
	c.Flags().StringVar(&host, "host", "", "sign-in hostname (default auth.<domain>)")
	c.Flags().StringVar(&owner, "owner", "", "your Google email; joins the @owners group that can open dev versions")
	return c
}

func authStatus() (*daemon.AuthStatus, error) {
	var st daemon.AuthStatus
	if err := call("auth.status", nil, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func cmdAuthStatus() *cobra.Command {
	return &cobra.Command{
		Use:   "auth:status",
		Short: "Show the sign-in provider, groups and which apps use sign-in",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := authStatus()
			if err != nil {
				return err
			}
			if jsonOut {
				printJSON(st)
				return nil
			}
			if st.Provider == "" {
				fmt.Println("Sign-in: not set up (berth auth:setup google)")
			} else {
				fmt.Printf("Sign-in:  %s at https://%s\nClient:   %s\n", st.Provider, st.Host, st.ClientID)
			}
			fmt.Printf("Sessions: %d (berth auth:sessions)\n", len(st.Sessions))
			fmt.Println("\nApps:")
			if len(st.Apps) == 0 {
				fmt.Println("  none (berth access <app> public|signed-in|allow ...)")
			}
			for _, name := range sortedKeys(st.Apps) {
				fmt.Printf("  %-14s %s\n", name, st.Apps[name])
			}
			fmt.Println("\nGroups:")
			if len(st.Groups) == 0 {
				fmt.Println("  none (berth auth:group <name> <email|@domain>...)")
			}
			for _, name := range sortedKeys(st.Groups) {
				fmt.Printf("  @%-13s %s\n", name, strings.Join(st.Groups[name], " "))
			}
			return nil
		},
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func cmdAuthGroup() *cobra.Command {
	var del bool
	c := &cobra.Command{
		Use:     "auth:group <name> [email|@domain...]",
		Short:   "Define a group for allow lists (@name); no members shows it",
		Example: "  berth auth:group family alice@gmail.com bob@gmail.com\n  berth access photos allow @family",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimPrefix(args[0], "@")
			if len(args) == 1 && !del {
				ac, err := store().AuthConfig()
				if err != nil {
					return err
				}
				m, ok := ac.Groups[name]
				if !ok {
					return fmt.Errorf("no group @%s (berth auth:status lists them)", name)
				}
				if jsonOut {
					printJSON(map[string]any{"group": name, "members": m})
					return nil
				}
				fmt.Println(strings.Join(m, "\n"))
				return nil
			}
			return call("auth.group", daemon.AuthGroupParams{Name: name, Members: args[1:], Delete: del}, nil)
		},
	}
	c.Flags().BoolVar(&del, "delete", false, "delete the group")
	return c
}

func cmdAuthSessions() *cobra.Command {
	return &cobra.Command{
		Use:   "auth:sessions",
		Short: "List signed-in sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := authStatus()
			if err != nil {
				return err
			}
			if jsonOut {
				printJSON(st.Sessions)
				return nil
			}
			if len(st.Sessions) == 0 {
				fmt.Println("Nobody is signed in.")
				return nil
			}
			fmt.Printf("%-32s %-32s %s\n", "EMAIL", "HOST", "SIGNED IN")
			for _, s := range st.Sessions {
				fmt.Printf("%-32s %-32s %s ago\n", s.Email, s.Host, time.Since(s.Created).Round(time.Minute))
			}
			return nil
		},
	}
}

func cmdAuthRevoke() *cobra.Command {
	return &cobra.Command{
		Use:   "auth:revoke <email>",
		Short: "Sign someone out of every app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call("auth.revoke", daemon.AuthRevokeParams{Email: args[0]}, nil)
		},
	}
}
