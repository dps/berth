package state

import (
	"encoding/json"
	"io"
	"os"
	"strings"
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

// TailLines returns the last n lines of a file without reading all of it.
func TailLines(path string, n int) ([]string, error) {
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
