// Package templates holds the plain-text files Berth renders: Dockerfiles
// for detected stacks, launchd plists, and the starter projects and agent
// instructions `berth new` writes. They are readable (and copyable)
// on purpose; `berth dockerfile <app>` prints the rendered result.
package templates

import "embed"

//go:embed *.Dockerfile *.plist new
var FS embed.FS
