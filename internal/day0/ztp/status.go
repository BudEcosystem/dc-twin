package ztp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// The twin-facing states (status.json's "state"). The twin's observer reads exactly these five
// (internal/adapter/netsim/tap, its day-0 observation): discovering and running are ZTP in
// progress, success and failed its outcome, and disabled is a switch whose ZTP will not run
// (turned off, already provisioned by hand, or skipped for a configuration it found).
const (
	stateDisabled    = "disabled"
	stateDiscovering = "discovering"
	stateRunning     = "running"
	stateSuccess     = "success"
	stateFailed      = "failed"
)

// twinStatus is /var/lib/dctwin/ztp/status.json, the client's report to the twin (18, "ZTP";
// principle 5, every step leaves an event). Source is where the script or document came from:
// local, usb, dhcp-239, dhcp-67, manual (Cumulus's `ztp -r`), or SONiC's own source names
// (local-fs, dhcp-opt67, dhcp-opt239). Attempts counts fetches (Cumulus's CUMULUS-PROV-COUNT;
// SONiC's document runs) and ExitCode is the last script's or plugin's (-1 before any ran).
type twinStatus struct {
	Flavour  string `json:"flavour"`
	State    string `json:"state"`
	Source   string `json:"source,omitempty"`
	URL      string `json:"url,omitempty"`
	Attempts int    `json:"attempts"`
	ExitCode int    `json:"exitCode"`
	Message  string `json:"message,omitempty"`
	Updated  string `json:"updated"`
}

// reporter keeps the status file and ZTP's log.
type reporter struct {
	c   *config
	st  twinStatus
	log *logger
}

func (r *reporter) set(state, msg string) {
	r.st.State, r.st.Message = state, msg
	r.save()
	if msg != "" {
		r.log.printf("state %s: %s", state, msg)
	}
}

func (r *reporter) save() {
	r.st.Updated = r.c.now().UTC().Format(time.RFC3339)
	if err := writeJSON(r.c.statusFile, r.st); err != nil {
		r.log.printf("cannot write %s: %v", r.c.statusFile, err)
	}
}

// readStatus is the status file as last written (zero when absent).
func (c *config) readStatus() twinStatus {
	var st twinStatus
	if b, err := os.ReadFile(c.statusFile); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

// logger writes ZTP's log lines, syslog-like (`<time> ztp[pid]: <message>`), to each of its
// files and, unless quiet, to stderr: Cumulus's ZTP logs to syslog and /var/lib/cumulus/ztp's
// ztp_state.log (A.6), SONiC's to /var/log/ztp.log (E.2).
type logger struct {
	mu    sync.Mutex
	now   func() time.Time
	files []string
	echo  io.Writer
}

func newLogger(c *config, echo bool, files ...string) *logger {
	l := &logger{now: c.now, files: files}
	if echo {
		l.echo = c.stderr
	}
	return l
}

func (l *logger) printf(format string, args ...any) {
	line := fmt.Sprintf("%s ztp[%d]: %s\n", l.now().UTC().Format("2006-01-02T15:04:05Z"), os.Getpid(), fmt.Sprintf(format, args...))
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.writer()
	defer w.Close()
	_, _ = io.WriteString(w, line)
}

// writer appends to every log file (and the echo), for a script's own output.
func (l *logger) writer() io.WriteCloser {
	var ws []io.Writer
	var fs []*os.File
	for _, p := range l.files {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			ws = append(ws, f)
			fs = append(fs, f)
		}
	}
	if l.echo != nil {
		ws = append(ws, l.echo)
	}
	return multiCloser{io.MultiWriter(ws...), fs}
}

type multiCloser struct {
	io.Writer
	fs []*os.File
}

func (m multiCloser) Close() error {
	for _, f := range m.fs {
		f.Close()
	}
	return nil
}

// lock takes ZTP's lock without waiting; held reports another run holding it.
func lock(path string) (release func(), held bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, true, nil
		}
		return nil, false, err
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); f.Close() }, false, nil
}

// runScript writes body to a temporary file (0700) and runs it as root, as both NOSes run a
// provisioning script: through its #! line when it has one, by /bin/sh otherwise, with args, on
// the client's PATH (the day-0 programs first) and, with --script-vrf, through `ip vrf exec`.
// Its stdout and stderr go to out. It returns the script's exit code, or -1 with the error when
// it could not be started.
func (c *config) runScript(ctx context.Context, body []byte, out io.Writer, env []string, args ...string) (int, error) {
	if err := os.MkdirAll(c.tmpDir, 0o755); err != nil {
		return -1, err
	}
	f, err := os.CreateTemp(c.tmpDir, "ztp-script-*")
	if err != nil {
		return -1, err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(body); err != nil {
		f.Close()
		return -1, err
	}
	f.Close()
	if err := os.Chmod(f.Name(), 0o700); err != nil {
		return -1, err
	}
	argv := []string{"/bin/sh", f.Name()}
	if strings.HasPrefix(string(body), "#!") {
		argv = []string{f.Name()}
	}
	return c.command(ctx, append(argv, args...), out, env)
}

// shell runs a command line given as a flag (--reload-cmd and the like) with /bin/sh -c, each
// arg shell-quoted and put in place of {file} or {} when the line has one, appended otherwise.
func (c *config) shell(ctx context.Context, line string, out io.Writer, args ...string) (int, error) {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	joined := strings.Join(q, " ")
	switch {
	case strings.Contains(line, "{file}"):
		line = strings.ReplaceAll(line, "{file}", joined)
	case strings.Contains(line, "{}"):
		line = strings.ReplaceAll(line, "{}", joined)
	case joined != "":
		line += " " + joined
	}
	return c.command(ctx, []string{"/bin/sh", "-c", line}, out, nil)
}

func (c *config) command(ctx context.Context, argv []string, out io.Writer, env []string) (int, error) {
	if c.scriptVRF != "" {
		argv = append([]string{c.ipCmd, "vrf", "exec", c.scriptVRF}, argv...)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Env = append(c.env(), env...)
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// env is the environment scripts run in: the client's own, with PATH replaced by c.path.
func (c *config) env() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "PATH=") {
			env = append(env, kv)
		}
	}
	return append(env, "PATH="+c.path)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
