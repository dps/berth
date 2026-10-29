//go:build !darwin

package daemon

import "context"

// readHostLoad is only implemented for macOS, where Berth runs.
func readHostLoad(ctx context.Context) hostLoad { return hostLoad{CPU: -1} }
