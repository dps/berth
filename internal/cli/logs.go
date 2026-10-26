package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/dps/berth/internal/state"
)

func cmdLogs() *cobra.Command {
	var follow, all, access, cronLog bool
	var n int
	var since, build, release string
	c := &cobra.Command{
		Use:   "logs [app]",
		Short: "Show app, build or access logs",
		Example: "  berth logs myapp -f\n" +
			"  berth logs myapp --since 1h\n" +
			"  berth logs myapp --build        # latest build\n" +
			"  berth logs myapp --build 12\n" +
			"  berth logs myapp --release      # latest release command (migrations)\n" +
			"  berth logs myapp --cron         # scheduled jobs from app.json\n" +
			"  berth logs --all -f",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := store()
			// `--build 12`: a flag with an optional value leaves 12 as an arg.
			if len(args) == 2 {
				switch {
				case build == "latest":
					build = args[1]
				case release == "latest":
					release = args[1]
				default:
					return fmt.Errorf("unexpected argument %q", args[1])
				}
				args = args[:1]
			}
			var cutoff time.Time
			if since != "" {
				d, err := time.ParseDuration(since)
				if err != nil {
					return fmt.Errorf("--since: %w", err)
				}
				cutoff = time.Now().Add(-d)
			}
			type src struct{ prefix, path string }
			var srcs []src
			switch {
			case all:
				apps, _ := s.Apps()
				width := 0
				for _, a := range apps {
					width = max(width, len(a.Name))
				}
				for _, a := range apps {
					srcs = append(srcs, src{fmt.Sprintf("%-*s | ", width, a.Name), s.Path("apps", a.Name, "logs", "app.log")})
				}
			case len(args) == 0:
				return fmt.Errorf("name an app, or use --all")
			default:
				app, err := appArg(args)
				if err != nil {
					return err
				}
				switch {
				case build != "":
					p, err := numberedLog(s.Path("apps", app, "logs"), "build", build)
					if err != nil {
						return err
					}
					srcs = append(srcs, src{"", p})
				case release != "":
					p, err := numberedLog(s.Path("apps", app, "logs"), "release", release)
					if err != nil {
						return err
					}
					srcs = append(srcs, src{"", p})
				case cronLog:
					srcs = append(srcs, src{"", s.Path("apps", app, "logs", "cron.log")})
				case access:
					logs := s.AccessLogs(app)
					if len(logs) == 0 {
						return fmt.Errorf("%s has no requests logged yet", app)
					}
					srcs = append(srcs, src{"", logs[len(logs)-1]})
				default:
					srcs = append(srcs, src{"", s.Path("apps", app, "logs", "app.log")})
				}
			}
			format := func(line string) (string, bool) {
				if access {
					return formatAccess(line, cutoff)
				}
				if !cutoff.IsZero() {
					if t, err := time.Parse(time.RFC3339, strings.SplitN(line, " ", 2)[0]); err == nil && t.Before(cutoff) {
						return "", false
					}
				}
				return line, true
			}
			// Backlog: last n lines from each source, merged by timestamp.
			var backlog []string
			for _, sr := range srcs {
				lines, _ := tailLines(sr.path, n)
				for _, l := range lines {
					if out, ok := format(l); ok {
						backlog = append(backlog, sr.prefix+out)
					}
				}
			}
			if all {
				sort.SliceStable(backlog, func(i, j int) bool { return stripPrefix(backlog[i]) < stripPrefix(backlog[j]) })
				if len(backlog) > n {
					backlog = backlog[len(backlog)-n:]
				}
			}
			for _, l := range backlog {
				fmt.Println(l)
			}
			if !follow {
				return nil
			}
			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
			defer cancel()
			lines := make(chan string, 256)
			for _, sr := range srcs {
				go followFile(ctx, sr.path, func(l string) {
					if out, ok := format(l); ok {
						lines <- sr.prefix + out
					}
				})
			}
			for {
				select {
				case <-ctx.Done():
					return nil
				case l := <-lines:
					fmt.Println(l)
				}
			}
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new lines")
	c.Flags().IntVarP(&n, "lines", "n", 200, "number of lines")
	c.Flags().StringVar(&since, "since", "", "only lines newer than this (e.g. 10m, 1h)")
	c.Flags().StringVar(&build, "build", "", "show a build log (latest, or release number)")
	c.Flags().Lookup("build").NoOptDefVal = "latest"
	c.Flags().StringVar(&release, "release", "", "show a release command's output (latest, or release number)")
	c.Flags().Lookup("release").NoOptDefVal = "latest"
	c.Flags().BoolVar(&access, "access", false, "show Caddy access logs")
	c.Flags().BoolVar(&cronLog, "cron", false, "show scheduled jobs' output (app.json cron)")
	c.Flags().BoolVar(&all, "all", false, "every app, prefixed with its name")
	return c
}

func stripPrefix(l string) string {
	if i := strings.Index(l, " | "); i >= 0 {
		return l[i+3:]
	}
	return l
}

// numberedLog finds <kind>-<n>.log (build or release) in dir.
func numberedLog(dir, kind, which string) (string, error) {
	if which != "latest" {
		n, err := strconv.Atoi(strings.TrimPrefix(which, "v"))
		if err != nil {
			return "", fmt.Errorf("--%s takes a release number", kind)
		}
		return filepath.Join(dir, fmt.Sprintf("%s-%d.log", kind, n)), nil
	}
	matches, _ := filepath.Glob(filepath.Join(dir, kind+"-*.log"))
	best, bestN := "", -1
	for _, m := range matches {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), kind+"-"), ".log"))
		if err == nil && n > bestN {
			best, bestN = m, n
		}
	}
	if best == "" {
		return "", fmt.Errorf("no %s logs yet", kind)
	}
	return best, nil
}

func formatAccess(line string, cutoff time.Time) (string, bool) {
	var e struct {
		TS      float64 `json:"ts"`
		Status  int     `json:"status"`
		Dur     float64 `json:"duration"`
		Size    int     `json:"size"`
		Request struct {
			ClientIP string `json:"client_ip"`
			Method   string `json:"method"`
			Host     string `json:"host"`
			URI      string `json:"uri"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return line, true
	}
	t := time.Unix(int64(e.TS), int64((e.TS-float64(int64(e.TS)))*1e9))
	if !cutoff.IsZero() && t.Before(cutoff) {
		return "", false
	}
	return fmt.Sprintf("%s %d %s %s%s %dB %.0fms %s", t.UTC().Format(time.RFC3339), e.Status, e.Request.Method, e.Request.Host, e.Request.URI, e.Size, e.Dur*1000, e.Request.ClientIP), true
}

// tailLines returns the last n lines of a file.
func tailLines(path string, n int) ([]string, error) { return state.TailLines(path, n) }

// followFile prints lines appended to path, surviving rotation.
func followFile(ctx context.Context, path string, emit func(string)) {
	var f *os.File
	var r *bufio.Reader
	var fi os.FileInfo
	open := func(fromEnd bool) {
		if f != nil {
			f.Close()
		}
		var err error
		f, err = os.Open(path)
		if err != nil {
			f = nil
			return
		}
		fi, _ = f.Stat()
		if fromEnd {
			f.Seek(0, io.SeekEnd)
		}
		r = bufio.NewReader(f)
	}
	open(true)
	partial := ""
	for {
		if f != nil {
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					partial += line
					break
				}
				emit(strings.TrimRight(partial+line, "\n"))
				partial = ""
			}
		}
		select {
		case <-ctx.Done():
			if f != nil {
				f.Close()
			}
			return
		case <-time.After(250 * time.Millisecond):
		}
		nfi, err := os.Stat(path)
		if err != nil {
			continue
		}
		if f == nil || !os.SameFile(fi, nfi) {
			open(false)
		}
	}
}
