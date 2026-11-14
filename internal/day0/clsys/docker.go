package clsys

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

// ExecRequest is what a factory SONiC switch's `docker exec` asks the twin (nos.ExecSocket): a
// command in one of the switch's service containers (lldp, bgp, …), which the twin runs there.
type ExecRequest struct {
	Container string   `json:"container"`
	Cmd       []string `json:"cmd"`
	// Log, in place of a command to run, is one a client ran over SSH (SSHCommand), to record
	Log *SSHLog `json:"log,omitempty"`
}

// ExecReply is the command's combined output and exit code, or why it did not run.
type ExecReply struct {
	Output string `json:"output"`
	Code   int    `json:"code"`
	Error  string `json:"error,omitempty"`
}

// Docker is SONiC's `docker`, as the host OS's CLI uses it: `docker exec [-i] [-t] [-u user]
// [-e k=v] <container> <cmd>…` runs the command in the switch's service container of that name
// (SONiC's show lldp neighbors runs `docker exec -i lldp lldpctl`). The VS image's services run as
// the twin's containers beside the switch, so the twin runs it; stdin is not forwarded. No other
// docker command is emulated.
func Docker(args []string) int {
	return docker(args, filepath.Join(nos.Day0Dir, nos.ExecSocket), os.Stdout, os.Stderr)
}

func docker(args []string, sock string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "exec" {
		fmt.Fprintf(stderr, "docker: the twin's docker emulates only exec (docker exec <container> <cmd>)\n")
		return 1
	}
	args = args[1:]
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch a := args[0]; {
		case a == "-i", a == "-t", a == "-it", a == "-ti", a == "--interactive", a == "--tty", a == "-d", a == "--detach":
			if a == "-d" || a == "--detach" {
				fmt.Fprintln(stderr, "docker exec: --detach is not emulated")
				return 1
			}
			args = args[1:]
		case a == "-u", a == "--user", a == "-e", a == "--env", a == "-w", a == "--workdir":
			if len(args) < 2 {
				fmt.Fprintf(stderr, "docker exec: %s needs a value\n", a)
				return 125
			}
			args = args[2:]
		case strings.HasPrefix(a, "--user="), strings.HasPrefix(a, "--env="), strings.HasPrefix(a, "--workdir="):
			args = args[1:]
		default:
			fmt.Fprintf(stderr, "docker exec: %s is not emulated\n", a)
			return 125
		}
	}
	if len(args) < 2 {
		fmt.Fprintln(stderr, "docker exec: requires a container and a command")
		return 125
	}
	c, err := net.DialTimeout("unix", sock, 5*time.Second)
	if err != nil {
		fmt.Fprintf(stderr, "Cannot connect to the Docker daemon: %v\n", err)
		return 1
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(ExecRequest{Container: args[0], Cmd: args[1:]}); err != nil {
		fmt.Fprintf(stderr, "docker exec: %v\n", err)
		return 1
	}
	var r ExecReply
	if err := json.NewDecoder(c).Decode(&r); err != nil {
		fmt.Fprintf(stderr, "docker exec: %v\n", err)
		return 1
	}
	if r.Error != "" {
		fmt.Fprintln(stderr, r.Error)
		return r.Code
	}
	fmt.Fprint(stdout, r.Output)
	return r.Code
}
