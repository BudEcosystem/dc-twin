//go:build linux

package tap

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/BudEcosystem/dc-twin/internal/day0/clsys"
	"github.com/BudEcosystem/dc-twin/internal/engine"
	"github.com/BudEcosystem/dc-twin/internal/nos"
	"github.com/BudEcosystem/dc-twin/internal/schema"
)

// serveExec takes a factory SONiC switch's `docker exec <container> <cmd>` (clsys.Docker) on the
// socket in its /etc/dctwin and runs the command in the switch's service container of that name,
// the lldp and bgp containers beside it, as SONiC's host CLI reaches its services.
func (s *Substrate) serveExec(sw *nosSwitch) error {
	dir, err := os.Open(s.day0Dir(sw.plan))
	if err != nil {
		return err
	}
	defer dir.Close()
	// bound through the directory's descriptor: the state directory's path can be longer than a
	// socket's path may be (108 bytes)
	at := fmt.Sprintf("/proc/self/fd/%d/%s", dir.Fd(), nos.ExecSocket)
	_ = os.Remove(filepath.Join(dir.Name(), nos.ExecSocket))
	l, err := net.Listen("unix", at)
	if err != nil {
		return fmt.Errorf("its docker exec socket: %w", err)
	}
	if err := os.Chmod(filepath.Join(dir.Name(), nos.ExecSocket), 0o666); err != nil {
		l.Close()
		return err
	}
	go func() {
		<-s.ctx.Done()
		l.Close()
	}()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go s.execOne(sw, c)
		}
	}()
	return nil
}

func (s *Substrate) execOne(sw *nosSwitch, c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Minute))
	var q clsys.ExecRequest
	reply := func(r clsys.ExecReply) { _ = json.NewEncoder(c).Encode(r) }
	if err := json.NewDecoder(c).Decode(&q); err != nil {
		reply(clsys.ExecReply{Code: 1, Error: "docker exec: " + err.Error()})
		return
	}
	if q.Log != nil {
		// the socket is the switch's docker daemon, open to its accounts; a record of an SSH
		// command is taken only from the sshd ForceCommand that ran it
		pid, ok := peerPID(c)
		if !ok || !sshCommandPeer(pid, procStat) {
			// loudly, not silently: a record the twin drops is a tool's doing it does not score,
			// and the switch's own ForceCommand must never be the one dropped
			s.log.Warn("a record of an SSH command came from something other than the switch's sshd ForceCommand",
				"switch", sw.plan.Device.Path, "pid", pid, "command", q.Log.Command)
			reply(clsys.ExecReply{Code: 1, Error: "dctwin: an SSH command's record is taken from the switch's sshd ForceCommand only"})
			return
		}
		s.recordSSH(sw, *q.Log)
		reply(clsys.ExecReply{})
		return
	}
	box := sw.sidecars[q.Container]
	if box == nil || len(q.Cmd) == 0 {
		reply(clsys.ExecReply{Code: 1, Error: "Error: No such container: " + q.Container})
		return
	}
	out, code, err := s.docker.Exec(s.ctx, box.ID, q.Cmd...)
	if err != nil {
		reply(clsys.ExecReply{Output: out, Code: 126, Error: "docker exec: " + err.Error()})
		return
	}
	reply(clsys.ExecReply{Output: out, Code: code})
}

// recordSSH records a command a client ran on a factory SONiC switch over SSH (clsys.SSHCommand)
// as the client's request on the switch: SONiC's CLI is how a day-0 tool reads and configures it,
// as NVUE's REST is on Cumulus (whose requests the twin's NVUE records). Its canonical name is
// sonicCanonical's; its status 200, or 500 for a command that exited non-zero.
func (s *Substrate) recordSSH(sw *nosSwitch, l clsys.SSHLog) {
	if s.eng == nil {
		return
	}
	canonical, method := sonicCanonical(l.Command)
	status := 200
	if l.Code != 0 {
		status = 500
	}
	s.eng.LogRead(engine.Request{Path: sw.plan.Device.Path, Canonical: canonical, Protocol: "ssh", Method: method, Target: l.Command,
		Source: schema.EventSource{Class: "stack", Principal: l.User, Address: l.Address}, Detail: map[string]string{"exit": strconv.Itoa(l.Code)}}, status)
}

// peerPID is the pid of the process at a Unix socket's other end (SO_PEERCRED), in the twin's pid
// namespace.
func peerPID(c net.Conn) (int, bool) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, false
	}
	var cred *unix.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) { cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil || cerr != nil {
		return 0, false
	}
	return int(cred.Pid), cred.Pid > 0
}

// procStat is a process's comm and parent, from /proc/<pid>/stat.
func procStat(pid int) (string, int, bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, false
	}
	open, end := bytes.IndexByte(b, '('), bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return "", 0, false
	}
	f := strings.Fields(string(b[end+1:]))
	if len(f) < 2 {
		return "", 0, false
	}
	ppid, err := strconv.Atoi(f[1])
	return string(b[open+1 : end]), ppid, err == nil
}
