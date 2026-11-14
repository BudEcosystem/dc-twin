package clsys

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// SSHCommand is a factory SONiC switch's sshd ForceCommand: every command a client sends over SSH
// runs as it would (the account's shell, -c), and once it has ended the twin is told who ran what,
// from where, and its exit code (an ExecRequest with Log), which it records as the client's
// request on the switch. A login with no command is the account's shell, unrecorded.
func SSHCommand(args []string) int {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := os.Getenv("SSH_ORIGINAL_COMMAND")
	if cmd == "" {
		c := exec.Command(shell, "-l")
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		return exitCode(c.Run())
	}
	c := exec.Command(shell, "-c", cmd)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	code := exitCode(c.Run())
	addr, _, _ := strings.Cut(os.Getenv("SSH_CONNECTION"), " ")
	reportSSH(filepath.Join(nos.Day0Dir, nos.ExecSocket), SSHLog{User: os.Getenv("USER"), Address: addr, Command: cmd, Code: code})
	return code
}

// SSHLog is one command a client ran over SSH.
type SSHLog struct {
	User    string `json:"user"`
	Address string `json:"address"`
	Command string `json:"command"`
	Code    int    `json:"code"`
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if x, ok := err.(*exec.ExitError); ok {
		return x.ExitCode()
	}
	return 127
}

// reportSSH tells the twin of a command, best effort: a switch the twin no longer hears from runs
// its commands all the same.
func reportSSH(sock string, l SSHLog) {
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_ = json.NewEncoder(c).Encode(ExecRequest{Log: &l})
	var r ExecReply
	_ = json.NewDecoder(c).Decode(&r)
}
