package state

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Event is one line of ~/.berth/events.log (JSON lines).
type Event struct {
	Time    time.Time      `json:"time"`
	Kind    string         `json:"kind"` // deploy.start, deploy.ok, deploy.failed, restart, crash, webhook, ...
	App     string         `json:"app,omitempty"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}

var eventMu sync.Mutex

const maxEventsLog = 10 << 20

// AppendEvent appends an event, rotating the log at 10 MB (keeps one old file).
func (s *Store) AppendEvent(ev Event) error {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	eventMu.Lock()
	defer eventMu.Unlock()
	p := s.EventsPath()
	if fi, err := os.Stat(p); err == nil && fi.Size() > maxEventsLog {
		os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}
