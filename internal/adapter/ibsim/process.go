package ibsim

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// console is what the adapter applies link state through: one ibsim console command, answered
// with what ibsim printed for it. The running ibsim process is one; tests give a recorder.
type console interface {
	Do(cmd string) (string, error)
}

// prompt is what ibsim prints after every console command (and once it is ready, after
// parsing the topology), with no newline; a response is everything printed between two.
const prompt = "sim> "

// process is one ibsim, started by the adapter, its console on a pipe the adapter holds: ibsim
// reads console commands from its standard input (sim_run selects on it beside its MAD
// sockets) and prints their output, then the prompt, on its standard output. Its standard
// error is libibmad's warnings. Both go to the transcript, with each command the adapter sent.
type process struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	resp    chan string
	exited  chan struct{}
	waitErr error

	mu       sync.Mutex // one command at a time
	log      *transcript
	stopping bool
	// quiet keeps a command's output out of the transcript (Dump, read for the SM's LIDs every
	// time UFM REST is asked for ports, would otherwise be most of it)
	quiet atomic.Bool
}

// transcript is the console log in the state directory, written by the reader, the stderr
// copier and the command writer.
type transcript struct {
	mu     sync.Mutex
	f      *os.File
	midway bool // the last byte written was not a newline (ibsim's prompt has none)
}

func (t *transcript) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(p) > 0 {
		t.midway = p[len(p)-1] != '\n'
	}
	return t.f.Write(p)
}

// printf writes a line of the adapter's own (a command it sent) on a line of its own.
func (t *transcript) printf(format string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.midway {
		t.f.WriteString("\n")
	}
	fmt.Fprintf(t.f, format, args...)
	t.midway = false
}

// startProcess runs ibsim on a topology file and waits for its first prompt: the network is
// parsed and started (-s) and its sockets are bound. banner is what it printed until then.
func startProcess(bin, netFile, sock string, sizes [3]int, logPath string) (p *process, banner string, err error) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, "", err
	}
	tr := &transcript{f: f}
	// -N, -S and -P raise ibsim's node, switch and port limits to what the topology needs
	// (never below its defaults, 2048, 256 and 13312)
	args := []string{"-s",
		"-N", fmt.Sprint(max(sizes[0], 2048)),
		"-S", fmt.Sprint(max(sizes[1], 256)),
		"-P", fmt.Sprint(max(sizes[2], 256*36+2048*2)),
		netFile}
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "IBSIM_SOCKNAME="+sock)
	cmd.SysProcAttr = sysProcAttr()
	cmd.Stderr = tr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		f.Close()
		return nil, "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		f.Close()
		return nil, "", err
	}
	tr.printf("$ IBSIM_SOCKNAME=%s %s %s\n", sock, bin, strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, "", err
	}
	p = &process{cmd: cmd, stdin: stdin, resp: make(chan string, 16), exited: make(chan struct{}), log: tr}
	go p.read(stdout)
	go func() {
		p.waitErr = cmd.Wait()
		close(p.exited)
	}()
	select {
	case banner = <-p.resp:
		return p, banner, nil
	case <-p.exited:
		f.Close()
		return nil, "", fmt.Errorf("ibsim exited before it was ready (%v); its transcript is %s", p.waitErr, logPath)
	case <-time.After(60 * time.Second):
		p.kill()
		return nil, "", fmt.Errorf("ibsim did not print its prompt in 60 s; its transcript is %s", logPath)
	}
}

// read splits ibsim's standard output at its prompt.
func (p *process) read(r io.Reader) {
	buf := make([]byte, 8192)
	var pending strings.Builder
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if !p.quiet.Load() {
				_, _ = p.log.Write(buf[:n])
			}
			pending.Write(buf[:n])
			s := pending.String()
			for {
				i := strings.Index(s, prompt)
				if i < 0 {
					break
				}
				select {
				case p.resp <- s[:i]:
				default: // nobody asked: unsolicited output, already in the transcript
				}
				s = s[i+len(prompt):]
			}
			pending.Reset()
			pending.WriteString(s)
		}
		if err != nil {
			return
		}
	}
}

// Do sends one console command and returns what ibsim printed for it.
func (p *process) Do(cmd string) (string, error) { return p.do(cmd, false) }

// doQuiet is Do with the output left out of the transcript (the command is logged).
func (p *process) doQuiet(cmd string) (string, error) { return p.do(cmd, true) }

func (p *process) do(cmd string, quiet bool) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if quiet {
		p.quiet.Store(true)
		defer p.quiet.Store(false)
	}
	for drained := false; !drained; { // a late answer to a command that timed out
		select {
		case <-p.resp:
		default:
			drained = true
		}
	}
	p.log.printf("> %s\n", cmd)
	if _, err := io.WriteString(p.stdin, cmd+"\n"); err != nil {
		return "", fmt.Errorf("ibsim console: %w", err)
	}
	select {
	case out := <-p.resp:
		return strings.TrimSpace(out), nil
	case <-p.exited:
		return "", fmt.Errorf("ibsim exited: %v", p.waitErr)
	case <-time.After(10 * time.Second):
		return "", errors.New("ibsim console: no prompt in 10 s")
	}
}

func (p *process) pid() int { return p.cmd.Process.Pid }

// running reports whether ibsim is still running.
func (p *process) running() bool {
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

// stop asks ibsim to quit (its console's Quit frees the network and exits) and kills it if it
// has not within three seconds.
func (p *process) stop() {
	p.mu.Lock()
	p.stopping = true
	p.log.printf("> Quit\n")
	_, _ = io.WriteString(p.stdin, "Quit\n")
	p.mu.Unlock()
	select {
	case <-p.exited:
	case <-time.After(3 * time.Second):
		p.kill()
	}
	_ = p.stdin.Close()
	p.log.mu.Lock()
	_ = p.log.f.Close()
	p.log.mu.Unlock()
}

func (p *process) kill() {
	_ = p.cmd.Process.Kill()
	<-p.exited
}

var versionRE = regexp.MustCompile(`(?m)ibsim (\d+\.\d+(?:\.\d+)?)\s*$`)

// ibsimVersion asks a binary its version: `ibsim -V` prints its usage and "<argv0> <version>",
// and exits non-zero.
func ibsimVersion(bin string) string {
	out, _ := exec.Command(bin, "-V").CombinedOutput()
	if m := versionRE.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return "unknown"
}
