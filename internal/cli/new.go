package cli

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/dps/berth/internal/state"
	"github.com/dps/berth/templates"
)

// ---- berth new (design.md, Starting a new app) ----

// stack is a starter project in templates/new/<name>.
type stack struct {
	About string // one line for AGENTS.md and README.md
	Local string // how to run it on a laptop
}

var stacks = map[string]stack{
	"node": {
		About: "A Node.js web app.",
		Local: "Run `npm run dev` (it restarts on changes) and open http://localhost:8080.\nAdd dependencies with `npm install <package>` and commit `package-lock.json`.",
	},
	"python": {
		About: "A Python web app, with FastAPI.",
		Local: "Run `uv run --with-requirements requirements.txt uvicorn main:app --reload --port 8080`\nand open http://localhost:8080. Add dependencies to `requirements.txt`.",
	},
	"go": {
		About: "A Go web app.",
		Local: "Run `go run .` and open http://localhost:8080.",
	},
	"static": {
		About: "A static website: Berth serves the files as they are.",
		Local: "Serve the folder with `python3 -m http.server 8080` and open http://localhost:8080.",
	},
}

const stackNames = "node, python, go or static"

// agentsData fills AGENTS.md, README.md and the starter files.
type agentsData struct {
	App    string
	Branch string
	Prod   string // production URL
	Dev    string // the dev version's URL
	SignIn bool
	AI     bool
	Stack  stack
}

func newAgentsData(app string, info *BerthInfo) agentsData {
	d := agentsData{App: app, Branch: "main"}
	switch {
	case info == nil:
		d.Prod, d.Dev = "https://"+app+".<your domain>", "https://"+app+"-dev.<your domain>"
	case info.Domain != "":
		d.Prod, d.Dev = fmt.Sprintf("https://%s.%s", app, info.Domain), fmt.Sprintf("https://%s-dev.%s", app, info.Domain)
	default: // LAN only
		d.Prod, d.Dev = fmt.Sprintf("https://%s.%s.local", app, info.Hostname), fmt.Sprintf("https://%s-dev.%s.local", app, info.Hostname)
	}
	if info != nil {
		d.SignIn, d.AI = info.SignIn, info.AI
	}
	return d
}

func newTemplate(name string) *template.Template {
	return template.Must(template.ParseFS(templates.FS, "new/"+name))
}

// renderAgents returns AGENTS.md, or with section "berth" only its Berth part.
func renderAgents(d agentsData, section string) string {
	var b bytes.Buffer
	if err := newTemplate("AGENTS.md.tmpl").ExecuteTemplate(&b, section, d); err != nil {
		panic(err) // the template is embedded, and covered by tests
	}
	return b.String()
}

// writeStarter writes the starter for d.Stack's files and the docs into dir.
func writeStarter(dir, stackName string, d agentsData) ([]string, error) {
	var files []string
	write := func(name string, body []byte) error {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		files = append(files, name)
		return os.WriteFile(p, body, 0o644)
	}
	root := "new/" + stackName
	err := fs.WalkDir(templates.FS, root, func(p string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}
		raw, err := fs.ReadFile(templates.FS, p)
		if err != nil {
			return err
		}
		var b bytes.Buffer
		if err := template.Must(template.New(p).Parse(string(raw))).Execute(&b, d); err != nil {
			return err
		}
		// Stored as gitignore and *.tmpl, so they don't act on Berth's own
		// repo and Go build.
		name := strings.TrimSuffix(strings.TrimPrefix(p, root+"/"), ".tmpl")
		if name == "gitignore" {
			name = ".gitignore"
		}
		return write(name, b.Bytes())
	})
	if err != nil {
		return nil, err
	}
	if err := write("AGENTS.md", []byte(renderAgents(d, "agents"))); err != nil {
		return nil, err
	}
	for _, name := range []string{"README.md", "CLAUDE.md"} {
		var b bytes.Buffer
		if err := newTemplate(name+".tmpl").Execute(&b, d); err != nil {
			return nil, err
		}
		if err := write(name, b.Bytes()); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// askYes asks a yes-or-no question that defaults to yes. Without a
// terminal to answer it, the answer is no.
func askYes(prompt string) bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false
	}
	fmt.Printf("%s [Y/n] ", prompt)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "" || line == "y" || line == "yes"
}

// githubUser returns the GitHub CLI's logged-in user, or "".
func githubUser() string {
	if _, err := exec.LookPath("gh"); err != nil {
		return ""
	}
	out, err := exec.Command("gh", "api", "user", "--jq", ".login").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func cmdNew() *cobra.Command {
	var stackName, repo string
	var public, noGitHub, noDeploy, yes bool
	c := &cobra.Command{
		Use:   "new <app>",
		Short: "Start a new app: a starter, AGENTS.md for coding agents, a GitHub repo, and its first deploy",
		Long: "Creates the folder <app> with a small web app that follows Berth's contract, AGENTS.md\n" +
			"(and CLAUDE.md) telling coding agents how to deploy, test and debug it on Berth, a README\n" +
			"and a first commit. Then, asking first: a GitHub repo through the GitHub CLI, the app on\n" +
			"Berth, and its first deploy.\n\n" +
			"Works on the Berth Mac or on another machine: there, set BERTH_HOST to the Mac's SSH\n" +
			"address and Berth commands run on the Mac.",
		Example: "  berth new myapp\n  berth new api --stack python\n  BERTH_HOST=me@mac-mini.local berth new myapp      # from a laptop",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app := args[0]
			if err := state.ValidateAppName(app); err != nil {
				return err
			}
			if strings.HasSuffix(app, "-dev") {
				return fmt.Errorf("names ending in -dev are for dev versions")
			}
			st, ok := stacks[stackName]
			if !ok {
				return fmt.Errorf("unknown stack %q: use %s", stackName, stackNames)
			}
			dir, err := filepath.Abs(app)
			if err != nil {
				return err
			}
			if _, err := os.Stat(dir); err == nil {
				return fmt.Errorf("%s already exists", app)
			}

			// Which Berth? This Mac, or the one BERTH_HOST names.
			var info *BerthInfo
			var target *sshTarget
			local := isBerthMac()
			if local {
				if info, err = localInfo(); err != nil {
					return err
				}
				if store().AppExists(app) {
					return fmt.Errorf("Berth already has an app named %s", app)
				}
				fmt.Printf("-----> Berth on this Mac (%s)\n", orLAN(info.Domain))
			} else if target, err = berthTarget(); err == nil {
				if info, err = remoteInfo(target); err != nil {
					var silent *silentExit
					if errors.As(err, &silent) {
						return hintErr(fmt.Errorf("couldn't ask the Berth on %s about itself", target), "the Mac's Berth may be older than this one: upgrade it there with `brew upgrade --fetch-HEAD berth`")
					}
					return err
				}
				fmt.Printf("-----> Berth on %s (%s)\n", target, orLAN(info.Domain))
			} else {
				fmt.Println("-----> No Berth here and BERTH_HOST isn't set: making the project only")
			}

			d := newAgentsData(app, info)
			d.Stack = st
			files, err := writeStarter(dir, stackName, d)
			if err != nil {
				return err
			}
			fmt.Printf("-----> Created %s/ (%s starter): %s\n", app, stackName, strings.Join(files, ", "))
			if err := os.Chdir(dir); err != nil {
				return err
			}
			for _, g := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-q", "-m", "Start " + app + " with berth new"}} {
				if out, err := exec.Command("git", g...).CombinedOutput(); err != nil {
					return fmt.Errorf("git %s: %s", g[0], strings.TrimSpace(string(out)))
				}
			}
			fmt.Println("-----> git init, first commit on main")

			// GitHub, through the GitHub CLI when it's logged in.
			owner := githubUser()
			if repo == "" && owner != "" {
				repo = owner + "/" + app
			}
			pushed := false
			if !noGitHub && owner != "" {
				vis := "private"
				if public {
					vis = "public"
				}
				if yes || askYes(fmt.Sprintf("Create a %s GitHub repo %s and push to it?", vis, repo)) {
					gh := exec.Command("gh", "repo", "create", repo, "--"+vis, "--source", ".", "--remote", "origin", "--push")
					gh.Stdout, gh.Stderr = os.Stdout, os.Stderr
					if err := gh.Run(); err != nil {
						return fmt.Errorf("gh repo create failed; the project is in %s", dir)
					}
					pushed = true
					fmt.Printf("-----> https://github.com/%s\n", repo)
				}
			}

			// The app on Berth, deploying from GitHub.
			created := false
			if pushed && info != nil && !noDeploy && (yes || askYes(fmt.Sprintf("Create %s on Berth and deploy it?", app))) {
				createArgs := []string{"create", app, "--repo", repo}
				if local {
					err = runSelf(createArgs...)
				} else {
					err = target.run(createArgs...)
				}
				if err != nil {
					return err
				}
				created = true
				if !local {
					// Lets berth commands in the repo find the Mac, and dev:push deploy from here.
					if err := addRemote(target, app, "", "berth"); err != nil {
						fmt.Printf("       couldn't add the berth remote: %v\n", err)
					}
				}
			}

			fmt.Println()
			r := orPlaceholder(repo, "<you>/"+app)
			switch {
			case created:
				fmt.Printf("%s is live at %s. Every push to main deploys it.\n", app, d.Prod)
			case !pushed:
				if owner == "" && !noGitHub {
					fmt.Println("To put it on GitHub (or install the GitHub CLI, `brew install gh && gh auth login`, and berth new does it for you):")
				} else {
					fmt.Println("To put it on GitHub:")
				}
				fmt.Printf("  1. Create an empty repository named %s at https://github.com/new (no README, licence or .gitignore)\n", path.Base(r))
				fmt.Printf("  2. cd %s && git remote add origin git@github.com:%s.git && git push -u origin main\n", app, r)
				fmt.Println("Then deploy it:")
				fallthrough
			default:
				if info == nil {
					fmt.Println("  export BERTH_HOST=<user>@<your-mac>.local     # the Berth Mac, as ssh reaches it")
				}
				fmt.Printf("  berth create %s --repo %s\n", app, r)
				if !local {
					fmt.Printf("  berth git:remote %s     # in %s/: berth commands there reach the Mac, and dev:push works\n", app, app)
				}
			}
			fmt.Printf("\nNext:\n  cd %s\n  claude       # or codex, cursor, …: AGENTS.md tells the agent how to work with Berth\n", app)
			if created {
				fmt.Printf("  berth dev:push               # try uncommitted changes at %s\n", d.Dev)
			}
			return nil
		},
	}
	c.Flags().StringVar(&stackName, "stack", "node", "starter: "+stackNames)
	c.Flags().StringVar(&repo, "repo", "", "GitHub repo to create (default <your GitHub user>/<app>)")
	c.Flags().BoolVar(&public, "public", false, "make the GitHub repo public (default private)")
	c.Flags().BoolVar(&noGitHub, "no-github", false, "don't create a GitHub repo")
	c.Flags().BoolVar(&noDeploy, "no-deploy", false, "don't create the app on Berth")
	c.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask: create the GitHub repo and the app")
	return c
}

// runSelf runs this berth binary with args, on this terminal.
func runSelf(args ...string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	c := exec.Command(exe, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		return &silentExit{code: 1}
	}
	return nil
}

func orLAN(domain string) string { return orPlaceholder(domain, "LAN only") }

func orPlaceholder(s, p string) string {
	if s == "" {
		return p
	}
	return s
}
