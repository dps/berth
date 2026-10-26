package cli

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/dps/berth/internal/daemon"
)

// ---- admin page (design.md, Admin page) ----

func cmdAdmin() *cobra.Command {
	var noBrowser bool
	c := &cobra.Command{
		Use:   "admin",
		Short: "Open the admin page (status, analytics, logs) in the browser",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var url string
			if isBerthMac() {
				ac, err := store().AuthConfig()
				if err != nil {
					return err
				}
				if ac.AdminHost == "" {
					return fmt.Errorf("the admin page is off: berth admin:enable")
				}
				url = "https://" + ac.AdminHost
			} else {
				// Ask the Mac, and open the page here.
				t, err := berthTarget()
				if err != nil {
					return err
				}
				out, err := t.output("admin", "--no-browser")
				if err != nil {
					return err
				}
				url = strings.TrimSpace(string(out))
			}
			fmt.Println(url)
			if !noBrowser {
				exec.Command("open", url).Run()
			}
			return nil
		},
	}
	c.Flags().BoolVar(&noBrowser, "no-browser", false, "just print the address")
	return c
}

func cmdAdminEnable() *cobra.Command {
	var p daemon.AdminParams
	c := &cobra.Command{
		Use:   "admin:enable",
		Short: "Serve the admin page at admin.<domain>, for the @owners group",
		Long: "The admin page shows everything berth status does, live, plus per-app analytics from\n" +
			"the access logs (requests, errors, latency, visitors, top pages, referrers, countries),\n" +
			"live requests, releases, logs and events. It's read-only, and only people in the\n" +
			"@owners sign-in group can open it.",
		Example: "  berth admin:enable --owner you@gmail.com\n  berth admin:enable --host dash.example.com",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if p.Owner == "" {
				if ac, err := store().AuthConfig(); err == nil && len(ac.Groups["owners"]) == 0 {
					p.Owner = ask("Your Google email (the admin page is for the @owners group): ")
				}
			}
			return call("admin.enable", p, nil)
		},
	}
	c.Flags().StringVar(&p.Host, "host", "", "hostname (default admin.<domain>)")
	c.Flags().StringVar(&p.Owner, "owner", "", "add this Google email to @owners")
	return c
}

func cmdAdminDisable() *cobra.Command {
	return &cobra.Command{
		Use:   "admin:disable",
		Short: "Stop serving the admin page",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return call("admin.enable", daemon.AdminParams{Disable: true}, nil)
		},
	}
}
