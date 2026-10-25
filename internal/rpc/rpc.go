// Package rpc is the CLI ↔ berthd protocol: HTTP over the unix socket at
// ~/.berth/berthd.sock. A call is POST /rpc/<method> with JSON params; the
// response is newline-delimited JSON messages: zero or more {"log": ...}
// lines streamed as the operation runs, then one final {"result": ...} or
// {"error": ...}.
package rpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Message struct {
	Log    string          `json:"log,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Done   bool            `json:"done,omitempty"`
}

// Stream is the server side of one call: an io.Writer for progress output.
type Stream struct {
	mu  sync.Mutex
	w   io.Writer
	f   http.Flusher
	buf []byte
}

func NewStream(w http.ResponseWriter) *Stream {
	f, _ := w.(http.Flusher)
	return &Stream{w: w, f: f}
}

// Write emits complete lines as log messages.
func (s *Stream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		s.send(Message{Log: string(s.buf[:i])})
		s.buf = s.buf[i+1:]
	}
	return len(p), nil
}

func (s *Stream) send(m Message) {
	b, _ := json.Marshal(m)
	s.w.Write(append(b, '\n'))
	if s.f != nil {
		s.f.Flush()
	}
}

// Finish flushes any partial line and sends the result or error.
func (s *Stream) Finish(result any, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) > 0 {
		s.send(Message{Log: string(s.buf)})
		s.buf = nil
	}
	m := Message{Done: true}
	if err != nil {
		m.Error = err.Error()
	} else if result != nil {
		b, merr := json.Marshal(result)
		if merr != nil {
			m.Error = merr.Error()
		} else {
			m.Result = b
		}
	}
	s.send(m)
}

// ---- client ----

type Client struct {
	Socket string
	http   *http.Client
}

func NewClient(socket string) *Client {
	return &Client{Socket: socket, http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}}}
}

var ErrDaemonDown = errors.New("berthd is not running (check `berth doctor`; start it with `launchctl kickstart gui/$UID/dev.berth.berthd`)")

// Ping checks the daemon answers.
func (c *Client) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := c.Call(ctx, "ping", nil, nil)
	return err
}

// Call invokes method, streaming log lines to logs (if non-nil), and
// decodes the result into out (if non-nil).
func (c *Client) Call(ctx context.Context, method string, params any, logs io.Writer, out ...any) (json.RawMessage, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://berthd/rpc/"+method, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "connect: no such file") || strings.Contains(err.Error(), "connection refused") {
			return nil, ErrDaemonDown
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("berthd: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var m Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		if m.Log != "" || (!m.Done && m.Error == "" && m.Result == nil) {
			if logs != nil {
				fmt.Fprintln(logs, m.Log)
			}
			continue
		}
		if m.Error != "" {
			return nil, errors.New(m.Error)
		}
		if len(out) > 0 && out[0] != nil && len(m.Result) > 0 {
			if err := json.Unmarshal(m.Result, out[0]); err != nil {
				return m.Result, err
			}
		}
		return m.Result, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("berthd closed the connection before finishing (see `berth events`)")
}
