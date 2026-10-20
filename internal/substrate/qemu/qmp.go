package qemu

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Event is one QMP event as QEMU emitted it (SHUTDOWN, POWERDOWN, RESET, STOP, RESUME …).
type Event struct {
	Name string         `json:"event"`
	Data map[string]any `json:"data,omitempty"`
	At   time.Time      `json:"-"`
}

// QMP is a client of one QEMU's machine protocol socket: commands in order, events on a
// channel. It is the substrate's control path and the observer's source of truth.
type QMP struct {
	conn    net.Conn
	mu      sync.Mutex // one command at a time
	replies chan qmpReply
	events  chan Event
	closed  chan struct{}
	Version string
}

type qmpReply struct {
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error"`
}

type qmpMessage struct {
	QMP *struct {
		Version struct {
			QEMU struct {
				Major, Minor, Micro int
			} `json:"qemu"`
		} `json:"version"`
	} `json:"QMP"`
	Event     string         `json:"event"`
	Data      map[string]any `json:"data"`
	Timestamp *struct {
		Seconds      int64 `json:"seconds"`
		Microseconds int64 `json:"microseconds"`
	} `json:"timestamp"`
	Return json.RawMessage `json:"return"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error"`
}

// DialQMP connects to a QMP socket, reads the greeting and leaves capabilities negotiation
// done, retrying until QEMU has created the socket, the deadline passes or abort closes (the
// process exited).
func DialQMP(path string, timeout time.Duration, abort <-chan struct{}) (*QMP, error) {
	deadline := time.Now().Add(timeout)
	var conn net.Conn
	var err error
	for {
		conn, err = net.DialTimeout("unix", path, time.Second)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("qmp %s: %w", path, err)
		}
		select {
		case <-abort:
			return nil, fmt.Errorf("qemu exited before its QMP socket answered")
		case <-time.After(50 * time.Millisecond):
		}
	}
	q := &QMP{conn: conn, replies: make(chan qmpReply, 1), events: make(chan Event, 256), closed: make(chan struct{})}
	dec := json.NewDecoder(conn)
	var greet qmpMessage
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	if err := dec.Decode(&greet); err != nil || greet.QMP == nil {
		conn.Close()
		return nil, fmt.Errorf("qmp %s: no greeting: %v", path, err)
	}
	v := greet.QMP.Version.QEMU
	q.Version = fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Micro)
	_ = conn.SetReadDeadline(time.Time{})
	go q.read(dec)
	if _, err := q.Execute("qmp_capabilities", nil); err != nil {
		q.Close()
		return nil, err
	}
	return q, nil
}

func (q *QMP) read(dec *json.Decoder) {
	defer close(q.closed)
	defer close(q.events)
	for {
		var m qmpMessage
		if err := dec.Decode(&m); err != nil {
			return
		}
		switch {
		case m.Event != "":
			ev := Event{Name: m.Event, Data: m.Data, At: time.Now()}
			if m.Timestamp != nil {
				ev.At = time.Unix(m.Timestamp.Seconds, m.Timestamp.Microseconds*1000)
			}
			select {
			case q.events <- ev:
			default: // an observer that does not keep up loses events, never blocks QEMU
			}
		default:
			select {
			case q.replies <- qmpReply{Return: m.Return, Error: m.Error}:
			case <-time.After(5 * time.Second):
			}
		}
	}
}

// Events is the event stream; it is closed when the connection ends (QEMU exited).
func (q *QMP) Events() <-chan Event { return q.events }

// Done is closed when the connection ends.
func (q *QMP) Done() <-chan struct{} { return q.closed }

// ErrClosed is returned for a command sent after QEMU went away.
var ErrClosed = errors.New("qmp: connection closed")

// Execute runs one command and returns its result.
func (q *QMP) Execute(cmd string, args map[string]any) (json.RawMessage, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	msg := map[string]any{"execute": cmd}
	if args != nil {
		msg["arguments"] = args
	}
	b, _ := json.Marshal(msg)
	_ = q.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := q.conn.Write(append(b, '\n')); err != nil {
		return nil, fmt.Errorf("qmp %s: %w", cmd, err)
	}
	select {
	case r := <-q.replies:
		if r.Error != nil {
			return nil, fmt.Errorf("qmp %s: %s: %s", cmd, r.Error.Class, r.Error.Desc)
		}
		return r.Return, nil
	case <-q.closed:
		return nil, ErrClosed
	case <-time.After(30 * time.Second):
		return nil, fmt.Errorf("qmp %s: no reply", cmd)
	}
}

// Status is query-status: running, paused, shutdown, prelaunch …
func (q *QMP) Status() (string, error) {
	r, err := q.Execute("query-status", nil)
	if err != nil {
		return "", err
	}
	var st struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(r, &st)
	return st.Status, nil
}

func (q *QMP) Close() error { return q.conn.Close() }
