package qemu

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// fakeQMP greets as QEMU does and answers every command with an empty return, sending each one
// it receives on cmds.
func fakeQMP(t *testing.T) (string, chan map[string]any) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "qmp.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	cmds := make(chan map[string]any, 8)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = c.Write([]byte(`{"QMP":{"version":{"qemu":{"major":11,"minor":1,"micro":2}},"capabilities":[]}}` + "\n"))
		sc := bufio.NewScanner(c)
		for sc.Scan() {
			var m map[string]any
			if json.Unmarshal(sc.Bytes(), &m) == nil {
				cmds <- m
			}
			_, _ = c.Write([]byte(`{"return":{}}` + "\n"))
		}
	}()
	t.Cleanup(func() { l.Close() })
	return sock, cmds
}

// SetLink is QMP set_link on a NIC's netdev: the guest's NIC loses or regains its carrier, as
// a pulled or replaced cable does to a real NIC.
func TestSetLink(t *testing.T) {
	sock, cmds := fakeQMP(t)
	q, err := DialQMP(sock, 2*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	<-cmds // qmp_capabilities
	if err := q.SetLink("net1", false); err != nil {
		t.Fatal(err)
	}
	m := <-cmds
	args, _ := m["arguments"].(map[string]any)
	if m["execute"] != "set_link" || args["name"] != "net1" || args["up"] != false {
		t.Fatalf("sent %v", m)
	}
}
