package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/dps/berth/internal/daemon"
	"github.com/dps/berth/internal/deploy"
)

// ---- off-Mac backups (design.md, Off-Mac backups) ----

// hidden reads a secret from the terminal without echoing it.
func hidden(prompt string) (string, error) {
	fmt.Print(prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		return strings.TrimSpace(string(b)), err
	}
	return ask(""), nil
}

func cmdOffsiteSetup() *cobra.Command {
	var p daemon.OffsiteParams
	var r2 string
	c := &cobra.Command{
		Use:   "offsite:setup",
		Short: "Copy an encrypted backup to S3-compatible storage (R2, B2, S3) every night",
		Long: "Every night after the database dumps, Berth encrypts the same archive `berth backup` makes\n" +
			"and uploads it to a bucket, keeping the newest 14. It's encrypted on this Mac with a key\n" +
			"only you hold, so the storage provider sees only ciphertext. Save the key setup prints:\n" +
			"without it the backups can't be read.\n\n" +
			"Cloudflare R2: create a bucket, then an R2 API token with Object Read & Write on it, and\n" +
			"pass your account ID with --r2. Other services: give --endpoint.",
		Example: "  berth offsite:setup --r2 <account id> --bucket my-berth-backups\n" +
			"  berth offsite:setup --endpoint https://s3.us-west-004.backblazeb2.com --bucket my-berth\n" +
			"  berth offsite:setup --r2 <account id> --bucket b --key berthrestore_…   # keep an earlier key",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if r2 != "" {
				p.Endpoint = "https://" + strings.TrimSpace(r2) + ".r2.cloudflarestorage.com"
			}
			if p.Endpoint == "" || p.Bucket == "" {
				return hintErr(fmt.Errorf("give the bucket and where it is: --r2 <account id> or --endpoint <url>, and --bucket"), "`berth offsite:setup --help` has examples")
			}
			if p.AccessKey == "" {
				p.AccessKey = strings.TrimSpace(ask("Access key ID: "))
			}
			if p.SecretKey = os.Getenv("BERTH_OFFSITE_SECRET"); p.SecretKey == "" {
				s, err := hidden("Secret access key (hidden): ")
				if err != nil {
					return err
				}
				p.SecretKey = s
			}
			var res daemon.OffsiteStatus
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if _, err := client().Call(ctx, "offsite.setup", p, logsWriter(), &res); err != nil {
				return err
			}
			if jsonOut {
				printJSON(res)
				return nil
			}
			fmt.Printf("-----> Off-Mac backups go to bucket %s, folder %s, every night after the 03:00 dumps (keeping %d)\n", res.Bucket, res.Prefix, res.Keep)
			printRecovery(res.Recovery)
			fmt.Println("Run one now with `berth offsite:run`.")
			return nil
		},
	}
	c.Flags().StringVar(&r2, "r2", "", "Cloudflare account ID, for an R2 bucket")
	c.Flags().StringVar(&p.Endpoint, "endpoint", "", "the S3 endpoint, e.g. https://s3.us-west-004.backblazeb2.com")
	c.Flags().StringVar(&p.Region, "region", "", "region (default auto, or us-east-1 for AWS)")
	c.Flags().StringVar(&p.Bucket, "bucket", "", "bucket name")
	c.Flags().StringVar(&p.Prefix, "prefix", "", "folder in the bucket (default berth/<mac>/)")
	c.Flags().StringVar(&p.AccessKey, "access-key-id", "", "access key ID (asked for if not given; the secret always is, or set BERTH_OFFSITE_SECRET)")
	c.Flags().IntVar(&p.Keep, "keep", 0, "backups to keep (default 14)")
	c.Flags().StringVar(&p.Key, "key", "", "an existing recovery code or key, to keep reading backups made with it")
	return c
}

func cmdOffsiteRun() *cobra.Command {
	return &cobra.Command{
		Use:   "offsite:run",
		Short: "Upload an encrypted backup now",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return call("offsite.run", nil, nil)
		},
	}
}

func cmdOffsiteStatus() *cobra.Command {
	return &cobra.Command{
		Use:     "offsite:status",
		Aliases: []string{"offsite"},
		Short:   "Where off-Mac backups go, the last upload, and the backups in the bucket",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var res daemon.OffsiteStatus
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if _, err := client().Call(ctx, "offsite.status", nil, nil, &res); err != nil {
				return err
			}
			if jsonOut {
				printJSON(res)
				return nil
			}
			if !res.SetUp {
				fmt.Println("Off-Mac backups aren't set up. `berth offsite:setup --help` shows how.")
				return nil
			}
			fmt.Printf("bucket:  %s, folder %s, at %s (access key %s), keeping %d\n", res.Bucket, res.Prefix, res.Endpoint, res.KeyID, res.Keep)
			switch l := res.Last; {
			case l.LastTry.IsZero():
				fmt.Println("last:    none yet (they run nightly; `berth offsite:run` runs one now)")
			case l.LastError != "":
				fmt.Printf("last:    FAILED %s: %s\n", ago(l.LastTry), l.LastError)
				if !l.LastOK.IsZero() {
					fmt.Printf("last ok: %s\n", ago(l.LastOK))
				}
			default:
				fmt.Printf("last:    ok %s, %s in %.0fs\n", ago(l.LastOK), deploy.HumanBytes(l.LastSize), l.LastSecs)
			}
			if res.ListErr != "" {
				fmt.Printf("\n !     couldn't list the bucket: %s\n", res.ListErr)
				return nil
			}
			if len(res.Backups) > 0 {
				fmt.Println()
				w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
				fmt.Fprintln(w, "BACKUP\tSIZE\tUPLOADED")
				for i := len(res.Backups) - 1; i >= 0; i-- {
					b := res.Backups[i]
					fmt.Fprintf(w, "%s\t%s\t%s\n", strings.TrimPrefix(b.Key, res.Prefix), deploy.HumanBytes(b.Size), ago(b.Modified))
				}
				w.Flush()
			}
			return nil
		},
	}
}

// printRecovery shows the recovery code with what it's for.
func printRecovery(code string) {
	if code == "" {
		return
	}
	fmt.Println()
	fmt.Println("Your recovery code holds the bucket's address and credentials and the key that")
	fmt.Println("encrypts the backups. Save it somewhere that isn't this Mac, such as a password")
	fmt.Println("manager. On a new Mac, `berth init --restore` asks for it and brings everything")
	fmt.Println("back. Without it, nobody can read the backups, including you.")
	fmt.Println()
	fmt.Println("    " + code)
	fmt.Println()
	fmt.Println("`berth offsite:key` shows it again. It changes if you run setup with new credentials.")
}

func cmdOffsiteKey() *cobra.Command {
	return &cobra.Command{
		Use:   "offsite:key",
		Short: "Print the recovery code for off-Mac backups (save it off this Mac)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			var res struct {
				Key      string `json:"key"`
				Recovery string `json:"recovery_code"`
			}
			if _, err := client().Call(context.Background(), "offsite.key", nil, nil, &res); err != nil {
				return err
			}
			if jsonOut {
				printJSON(res)
				return nil
			}
			if res.Recovery == "" {
				fmt.Println(res.Key)
				return nil
			}
			fmt.Println(res.Recovery)
			return nil
		},
	}
}

func cmdOffsiteGet() *cobra.Command {
	var p daemon.OffsiteParams
	c := &cobra.Command{
		Use:   "offsite:get [backup|latest]",
		Short: "Download and decrypt a backup, ready for berth restore",
		Example: "  berth offsite:get                      # the latest, into your home folder\n" +
			"  berth offsite:get berth-20261025-030012.tar.gz.enc --to /tmp/b.tar.gz\n" +
			"  berth offsite:get --key berthrestore_…  # with a key this Mac doesn't have\n" +
			"  berth restore ~/berth-20261025-030012.tar.gz",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				p.Object = args[0]
			}
			return call("offsite.get", p, nil)
		},
	}
	c.Flags().StringVar(&p.Dest, "to", "", "where to write the .tar.gz on the Mac (default your home folder)")
	c.Flags().StringVar(&p.Key, "key", "", "the recovery code (or key), if this Mac doesn't have it")
	return c
}

func cmdOffsiteOff() *cobra.Command {
	return &cobra.Command{
		Use:   "offsite:off",
		Short: "Stop off-Mac backups (the bucket and its backups stay)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return call("offsite.off", nil, nil)
		},
	}
}
