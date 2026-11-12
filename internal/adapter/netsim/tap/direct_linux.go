//go:build linux

package tap

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Reading a switch without exec. Every `exec` into a container is a session its runtime keeps
// (rootless Podman: a conmon and a cleanup per session, serialised through one database), and an
// observer that execs every few seconds into every switch piles them up on a loaded host. The
// twin runs on the Docker host beside the containers (it opens their network namespaces already),
// so it reads what an observer needs from the container itself: its files through
// /proc/<pid>/root, its daemons through their own unix sockets there.

// containerFile reads a file inside a container, by its init process's root.
func containerFile(pid int, path string) ([]byte, error) {
	if pid <= 0 {
		return nil, errors.New("no container process")
	}
	return os.ReadFile(filepath.Join(fmt.Sprintf("/proc/%d/root", pid), path))
}

// frrSocketDirs are where FRR keeps its daemons' vty sockets in the images the twin runs: /run
// first, as /var/run is an absolute link to it, which /proc/<pid>/root resolves on the host.
var frrSocketDirs = []string{"/run/frr", "/var/run/frr"}

// vtyshCommand runs a command on one FRR daemon (bgpd, zebra) over its vty socket, as vtysh does:
// the command NUL-terminated; the reply ends with three NULs and a status byte (0: success).
func vtyshCommand(ctx context.Context, pid int, daemon, cmd string) (string, error) {
	var conn net.Conn
	var err error
	for _, dir := range frrSocketDirs {
		p := filepath.Join(fmt.Sprintf("/proc/%d/root", pid), dir, daemon+".vty")
		var d net.Dialer
		if conn, err = d.DialContext(ctx, "unix", p); err == nil {
			break
		}
	}
	if conn == nil {
		return "", fmt.Errorf("%s's vty socket: %w", daemon, err)
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(10 * time.Second)
	}
	_ = conn.SetDeadline(deadline)
	// a vty starts unprivileged; vtysh enables it first
	for _, c := range []string{"enable", cmd} {
		if _, err := conn.Write(append([]byte(c), 0)); err != nil {
			return "", err
		}
		out, status, err := readVty(conn)
		if err != nil {
			return "", err
		}
		if c == cmd {
			if status != 0 {
				return out, fmt.Errorf("%s: %q: status %d", daemon, cmd, status)
			}
			return out, nil
		}
	}
	return "", nil
}

// readVty reads one reply: everything up to three NULs, then the status byte.
func readVty(conn net.Conn) (string, byte, error) {
	var b bytes.Buffer
	buf := make([]byte, 64<<10)
	for {
		n, err := conn.Read(buf)
		b.Write(buf[:n])
		if d := b.Bytes(); len(d) >= 4 && d[len(d)-4] == 0 && d[len(d)-3] == 0 && d[len(d)-2] == 0 {
			return string(d[:len(d)-4]), d[len(d)-1], nil
		}
		if err != nil {
			return b.String(), 0, err
		}
	}
}

// redisHGet reads one field of a hash from a SONiC switch's redis (CONFIG_DB is database 4), over
// its unix socket, in RESP.
func redisHGet(ctx context.Context, pid, db int, key, field string) (string, error) {
	// /run first: a container's /var/run is an absolute link to /run, which through
	// /proc/<pid>/root resolves against the host's root, not the container's
	var conn net.Conn
	var err error
	for _, sock := range []string{"/run/redis/redis.sock", "/var/run/redis/redis.sock"} {
		var d net.Dialer
		if conn, err = d.DialContext(ctx, "unix", filepath.Join(fmt.Sprintf("/proc/%d/root", pid), sock)); err == nil {
			break
		}
	}
	if conn == nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	cmd := func(args ...string) string {
		var s strings.Builder
		fmt.Fprintf(&s, "*%d\r\n", len(args))
		for _, a := range args {
			fmt.Fprintf(&s, "$%d\r\n%s\r\n", len(a), a)
		}
		return s.String()
	}
	if _, err := conn.Write([]byte(cmd("SELECT", strconv.Itoa(db)) + cmd("HGET", key, field))); err != nil {
		return "", err
	}
	r := bufio.NewReader(conn)
	if l, err := r.ReadString('\n'); err != nil || !strings.HasPrefix(l, "+OK") {
		return "", fmt.Errorf("redis SELECT %d: %q %v", db, l, err)
	}
	l, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	l = strings.TrimRight(l, "\r\n")
	if l == "$-1" {
		return "", nil
	}
	if !strings.HasPrefix(l, "$") {
		return "", fmt.Errorf("redis HGET: %q", l)
	}
	n, _ := strconv.Atoi(l[1:])
	v := make([]byte, n+2)
	if _, err := ioReadFull(r, v); err != nil {
		return "", err
	}
	return string(v[:n]), nil
}

func ioReadFull(r *bufio.Reader, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := r.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
