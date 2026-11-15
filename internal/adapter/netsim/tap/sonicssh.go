package tap

import (
	"path"
	"strings"
)

// The canonical names of what a SONiC CLI command does, strongest first: a reset to the factory
// (config-setup factory, ztp run), a configuration loaded, one saved, the NOS image installed, a
// reboot, CONFIG_DB or FRR changed directly, an account written, a file staged on the switch; else
// a read of LLDP's neighbours, of FRR's state, or of the switch's configuration and state.
var sonicRank = map[string]int{
	"network.config.reset": 90, "network.config.apply": 80, "network.config.save": 70, "network.firmware.update": 60,
	"network.reboot": 50, "network.config.change": 40, "network.account.write": 30, "network.config.stage": 20,
	"network.lldp.read": 12, "network.bgp.read": 11, "network.config.read": 10,
}

// sonicCanonical names a SONiC CLI command as a canonical request: the strongest thing any of its
// commands does (a compound command, `a && b`, `a | b`, is its strongest part), read word by word
// (a command is its program and arguments, past sudo, docker exec and sh -c), not by substring: a
// read whose words name a write (show reboot-cause) is a read.
func sonicCanonical(cmd string) (canonical, method string) {
	canonical, method = "network.config.read", ""
	for _, seg := range shellCommands(cmd, 0) {
		c, m := sonicCommand(seg)
		if method == "" || sonicRank[c] > sonicRank[canonical] {
			canonical, method = c, m
		}
	}
	return canonical, method
}

// shellSeg is one simple command: its words, and whether it redirects its output to a file.
type shellSeg struct {
	words []string
	write bool
}

// shellCommands splits a command line into its simple commands (on ;, &, &&, ||, | and newlines,
// outside quotes), each one's words unquoted, past the wrappers that run another command: sudo and
// its options, env assignments, docker exec <container>, and sh|bash -c '<command line>'.
func shellCommands(line string, depth int) []shellSeg {
	var segs []shellSeg
	var cur shellSeg
	var word strings.Builder
	inWord, redirect, input := false, false, false
	flush := func() {
		if !inWord {
			return
		}
		w := word.String()
		word.Reset()
		inWord = false
		if input {
			input = false
			return
		}
		if redirect {
			redirect = false
			if w != "/dev/null" && !strings.HasPrefix(w, "&") {
				cur.write = true
			}
			return
		}
		cur.words = append(cur.words, w)
	}
	end := func() {
		flush()
		if len(cur.words) > 0 || cur.write {
			segs = append(segs, cur)
		}
		cur = shellSeg{}
	}
	for i := 0; i < len(line); i++ {
		ch := line[i]
		switch {
		case ch == '\'':
			inWord = true
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				j = len(line) - i - 1
			}
			word.WriteString(line[i+1 : i+1+j])
			i += j + 1
		case ch == '"':
			inWord = true
			for i++; i < len(line) && line[i] != '"'; i++ {
				if line[i] == '\\' && i+1 < len(line) {
					i++
				}
				word.WriteByte(line[i])
			}
		case ch == '\\' && i+1 < len(line):
			inWord = true
			i++
			word.WriteByte(line[i])
		case ch == ' ' || ch == '\t':
			flush()
		case ch == ';' || ch == '&' || ch == '|' || ch == '\n':
			if ch == '&' && i > 0 && line[i-1] == '>' {
				// >&2: a descriptor, not a file
				redirect, inWord = true, true
				word.WriteByte('&')
				continue
			}
			end()
		case ch == '>' || ch == '<':
			// a redirect: N>, >>, >file; its target is the next word. An output to a file other
			// than /dev/null is a write
			w := word.String()
			if inWord && strings.Trim(w, "0123456789") == "" {
				word.Reset()
				inWord = false
			} else {
				flush()
			}
			if i+1 < len(line) && line[i+1] == '>' {
				i++
			}
			redirect, input = ch == '>', ch == '<'
		default:
			inWord = true
			word.WriteByte(ch)
		}
	}
	end()
	var out []shellSeg
	for _, s := range segs {
		s.words = unwrap(s.words)
		if len(s.words) >= 3 && depth < 4 && (path.Base(s.words[0]) == "sh" || path.Base(s.words[0]) == "bash") && s.words[1] == "-c" {
			out = append(out, shellCommands(s.words[2], depth+1)...)
			continue
		}
		out = append(out, s)
	}
	return out
}

// unwrap is a command's words past sudo (and its options), env assignments and docker exec.
func unwrap(w []string) []string {
	for len(w) > 0 {
		switch p := path.Base(w[0]); {
		case strings.Contains(w[0], "=") && !strings.HasPrefix(w[0], "-"):
			w = w[1:]
		case p == "sudo":
			w = w[1:]
		opts:
			for len(w) > 0 && strings.HasPrefix(w[0], "-") {
				switch w[0] {
				case "-p", "--prompt", "-u", "--user", "-g", "-C", "-h", "-r", "-t", "-U":
					w = w[min(2, len(w)):]
				case "--":
					w = w[1:]
					break opts
				default:
					w = w[1:]
				}
			}
		case p == "docker" && len(w) > 1 && w[1] == "exec":
			w = w[2:]
			for len(w) > 0 && strings.HasPrefix(w[0], "-") {
				switch w[0] {
				case "-u", "--user", "-e", "--env", "-w", "--workdir":
					w = w[min(2, len(w)):]
				default:
					w = w[1:]
				}
			}
			if len(w) > 0 {
				w = w[1:] // the container
			}
		default:
			return w
		}
	}
	return w
}

// sonicCommand names one simple command.
func sonicCommand(s shellSeg) (canonical, method string) {
	if len(s.words) == 0 {
		return "network.config.stage", ">"
	}
	prog := path.Base(s.words[0])
	args := s.words[1:]
	sub := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			sub = a
			break
		}
	}
	has := func(xs ...string) bool {
		for _, a := range args {
			for _, x := range xs {
				if strings.EqualFold(a, x) {
					return true
				}
			}
		}
		return false
	}
	switch prog {
	case "config-setup":
		if sub == "factory" {
			return "network.config.reset", "config-setup factory"
		}
	case "ztp":
		if sub == "run" {
			return "network.config.reset", "ztp run"
		}
	case "config":
		switch sub {
		case "":
		case "reload", "load", "load_minigraph", "load_mgmt_config":
			return "network.config.apply", "config " + sub
		case "save":
			return "network.config.save", "config save"
		default:
			return "network.config.change", "config " + sub
		}
	case "sonic-installer", "sonic_installer":
		if sub == "install" {
			return "network.firmware.update", "sonic-installer install"
		}
	case "reboot", "fast-reboot", "warm-reboot", "soft-reboot":
		return "network.reboot", prog
	case "systemctl":
		if sub == "reboot" {
			return "network.reboot", "systemctl reboot"
		}
	case "sonic-db-cli", "redis-cli":
		if has("del", "hset", "hdel", "set", "hmset", "flushdb", "flushall", "expire", "rename") {
			return "network.config.change", prog
		}
	case "sonic-cfggen":
		if has("--write-to-db", "-w") {
			return "network.config.change", prog
		}
	case "vtysh":
		for i, a := range args {
			if a == "-f" || (a == "-c" && i+1 < len(args) && strings.HasPrefix(strings.ToLower(strings.TrimSpace(args[i+1])), "conf")) {
				return "network.config.change", "vtysh"
			}
		}
		if !s.write {
			return "network.bgp.read", "vtysh"
		}
	case "useradd", "userdel", "usermod", "chpasswd", "passwd", "groupadd", "gpasswd":
		return "network.account.write", prog
	case "tee":
		for _, a := range args {
			if !strings.HasPrefix(a, "-") && a != "/dev/null" {
				return "network.config.stage", "tee"
			}
		}
	case "cp", "mv", "install":
		return "network.config.stage", prog
	case "scp":
		// scp's remote end: -t receives a file, -f sends one
		if has("-t") {
			return "network.config.stage", "scp"
		}
	case "lldpctl", "lldpcli":
		if !s.write {
			return "network.lldp.read", prog
		}
	case "show":
		if sub == "lldp" && !s.write {
			return "network.lldp.read", "show lldp"
		}
	}
	if s.write {
		return "network.config.stage", prog + " >"
	}
	return "network.config.read", prog
}

// sshCommandPeer reports whether the process that sent an SSH command's record (pid, from its
// socket's peer credentials) is the switch's sshd ForceCommand: dctwin-ssh-command, whose nearest
// ancestor other than a shell (sshd runs a ForceCommand through the account's shell) is sshd. A
// record from anything else on the switch, which could name any command and client, is not taken.
// proc reads a process's name (its comm, at most 15 bytes) and its parent.
func sshCommandPeer(pid int, proc func(pid int) (comm string, ppid int, ok bool)) bool {
	comm, ppid, ok := proc(pid)
	if !ok || !strings.HasPrefix("dctwin-ssh-command", comm) || len(comm) < len("dctwin-ssh-com") {
		return false
	}
	for hops := 0; hops < 3 && ppid > 1; hops++ {
		if comm, ppid, ok = proc(ppid); !ok {
			return false
		}
		switch comm {
		case "sh", "bash", "dash", "zsh":
			continue
		case "sshd", "sshd-session":
			return true
		}
		return false
	}
	return false
}
