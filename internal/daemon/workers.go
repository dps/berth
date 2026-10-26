package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/dps/berth/internal/state"
)

type ScaleParams struct {
	App   string `json:"app"`
	Type  string `json:"type"`
	Count int    `json:"count"`
}

func (d *Daemon) hScale(ctx context.Context, params json.RawMessage, out io.Writer) (any, error) {
	var p ScaleParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	if !d.Store.AppExists(p.App) {
		return nil, fmt.Errorf("no app named %q", p.App)
	}
	if err := d.Deployer.Scale(ctx, p.App, p.Type, p.Count, out); err != nil {
		return nil, err
	}
	d.event(state.Event{Kind: "scale", App: p.App, Message: fmt.Sprintf("%s=%d", p.Type, p.Count)})
	a, _ := d.Store.App(p.App)
	return map[string]any{"app": p.App, "scale": a.Scale}, nil
}
