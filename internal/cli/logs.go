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
)

func cmdLogs() *cobra.Command {
	var follow, all, access bool
	var n int
	var since, build string
	c := &cobra.Command{
		Use:   "logs [app]",
		Short: "Show app, build or access logs",
		Example: "  berth logs myapp -f\n" +
			"  berth logs myapp --since 1h\n" +
			"  berth logs myapp --build        # latest build\n" +
			"  berth logs myapp --build 12\n" +
			"  berth logs --all -f",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := store()
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
					p, err := buildLogPath(s.Path("apps", app, "logs"), build)
					if err != nil {
						return err
					}
					srcs = append(srcs, src{"", p})
				case access:
					srcs = append(srcs, src{"", s.Path("apps", app, "logs", "access.log")})
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
	c.Flags().BoolVar(&access, "access", false, "show Caddy access logs")
	c.Flags().BoolVar(&all, "all", false, "every app, prefixed with its name")
	return c
}

func stripPrefix(l string) string {
	if i := strings.Index(l, " | "); i >= 0 {
		return l[i+3:]
	}
	return l
}

func buildLogPath(dir, which string) (string, error) {
	if which != "latest" {
		n, err := strconv.Atoi(strings.TrimPrefix(which, "v"))
		if err != nil {
			return "", fmt.Errorf("--build takes a release number")
		}
		return filepath.Join(dir, fmt.Sprintf("build-%d.log", n)), nil
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "build-*.log"))
	best, bestN := "", -1
	for _, m := range matches {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(m), "build-"), ".log"))
		if err == nil && n > bestN {
			best, bestN = m, n
		}
	}
	if best == "" {
		return "", fmt.Errorf("no build logs yet")
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
func tailLines(path string, n int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	var buf []byte
	const block = 64 * 1024
	off := size
	for off > 0 && strings.Count(string(buf), "\n") <= n {
		step := min(int64(block), off)
		off -= step
		chunk := make([]byte, step)
		if _, err := f.ReadAt(chunk, off); err != nil && err != io.EOF {
			return nil, err
		}
		buf = append(chunk, buf...)
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // partial first line
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	if len(lines) == 1 && lines[0] == "" {
		return nil, nil
	}
	return lines, nil
}

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
