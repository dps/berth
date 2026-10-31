package analytics

import (
	"bufio"
	"io"
	"os"
	"syscall"
	"time"
)

// Tailer feeds Stats from the access logs, picking up where it left off.
// Files are tracked by inode, so a log Caddy rotates (renames) isn't read
// twice, and a truncated file is read again from the start.
type Tailer struct {
	Stats *Stats
	// Files lists each app's access logs, oldest first.
	Files func() map[string][]string
	// OnRequest, if set, sees each request after it's added to Stats.
	OnRequest func(app string, r Request)
	offsets   map[uint64]int64
}

func NewTailer(stats *Stats, files func() map[string][]string) *Tailer {
	return &Tailer{Stats: stats, Files: files, offsets: map[uint64]int64{}}
}

// Poll reads whatever was appended since the last poll.
func (t *Tailer) Poll() {
	cut := t.Stats.Now().Add(-Keep)
	for app, files := range t.Files() {
		for _, path := range files {
			fi, err := os.Stat(path)
			if err != nil || fi.ModTime().Before(cut) {
				continue
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				continue
			}
			ino := st.Ino
			off := t.offsets[ino]
			if fi.Size() < off {
				off = 0 // truncated
			}
			if fi.Size() == off {
				continue
			}
			t.offsets[ino] = t.read(app, path, off)
		}
	}
}

// read adds the complete lines from off on and returns the offset after
// the last one (a line still being written is read next time).
func (t *Tailer) read(app, path string, off int64) int64 {
	f, err := os.Open(path)
	if err != nil {
		return off
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return off
	}
	rd := bufio.NewReaderSize(f, 256<<10)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			return off // partial or no line: leave it for next time
		}
		off += int64(len(line))
		if r, ok := Parse(line); ok {
			t.Stats.Add(app, r)
			if t.OnRequest != nil {
				t.OnRequest(app, r)
			}
		}
	}
}

// Run polls every interval until stop is closed, pruning old data once a
// minute.
func (t *Tailer) Run(every time.Duration, stop <-chan struct{}) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	last := time.Now()
	t.Poll()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			t.Poll()
			if time.Since(last) > time.Minute {
				t.Stats.Prune()
				last = time.Now()
			}
		}
	}
}
