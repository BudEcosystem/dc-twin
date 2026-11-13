package clsys

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// Sudo is sudo on a factory switch whose image has none (sonic-vs): installed setuid root, it runs
// the command as root, as sudo does for a member of the sudo group (SONiC's admin and the accounts
// a day-0 tool adds to it). The options a tool sends are taken: -S reads the password from stdin's
// first line, which the command does not see (it is not checked: the twin does not keep the
// switch's PAM); -p takes its prompt; -n, -E, -H and their like change nothing here; -u root is
// the only user. Any other option is refused by name.
func Sudo(args []string) int {
	return sudo(args, os.Stdin, os.Stderr, func(cmd []string, stdin io.Reader) int {
		if err := syscall.Setgid(0); err != nil {
			fmt.Fprintf(os.Stderr, "sudo: %v (is it installed setuid root?)\n", err)
			return 1
		}
		if err := syscall.Setuid(0); err != nil {
			fmt.Fprintf(os.Stderr, "sudo: %v (is it installed setuid root?)\n", err)
			return 1
		}
		path, err := exec.LookPath(cmd[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "sudo: %s: command not found\n", cmd[0])
			return 1
		}
		c := exec.Command(path, cmd[1:]...)
		c.Stdin, c.Stdout, c.Stderr = stdin, os.Stdout, os.Stderr
		c.Env = append(os.Environ(), "SUDO_USER="+os.Getenv("USER"), "USER=root", "LOGNAME=root", "HOME=/root")
		if err := c.Run(); err != nil {
			if x, ok := err.(*exec.ExitError); ok {
				return x.ExitCode()
			}
			fmt.Fprintf(os.Stderr, "sudo: %v\n", err)
			return 1
		}
		return 0
	})
}

func sudo(args []string, stdin io.Reader, stderr io.Writer, run func([]string, io.Reader) int) int {
	in := bufio.NewReader(stdin)
	for len(args) > 0 && strings.HasPrefix(args[0], "-") {
		switch a := args[0]; a {
		case "-S", "--stdin":
			_, _ = in.ReadString('\n')
			args = args[1:]
		case "-p", "--prompt", "-g", "-C", "-h", "-r", "-t", "-U":
			if len(args) < 2 {
				fmt.Fprintf(stderr, "sudo: option requires an argument -- %s\n", a)
				return 1
			}
			args = args[2:]
		case "-u", "--user":
			if len(args) < 2 || (args[1] != "root" && args[1] != "0" && args[1] != "#0") {
				fmt.Fprintln(stderr, "sudo: the twin's sudo runs commands as root only")
				return 1
			}
			args = args[2:]
		case "-n", "-E", "-H", "-k", "-K", "-b", "-P", "-A", "--non-interactive", "--preserve-env":
			args = args[1:]
		case "--":
			args = args[1:]
			goto cmd
		default:
			fmt.Fprintf(stderr, "sudo: %s: the twin's sudo does not emulate this option\n", a)
			return 1
		}
	}
cmd:
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: sudo command")
		return 1
	}
	return run(args, in)
}
