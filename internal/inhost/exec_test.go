package inhost

import (
	"bufio"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/engine"
)

// The twin asks the agent to run a script with an exec line, the script base64 on one line short
// enough for the tty's line buffer; the agent answers with its exit code and output.
func TestExecLines(t *testing.T) {
	l, err := ExecLine(7, "ip -br addr")
	if err != nil || l != "@@dc-twin exec 7 "+base64.StdEncoding.EncodeToString([]byte("ip -br addr"))+"\n" {
		t.Fatalf("ExecLine = %q, %v", l, err)
	}
	if _, err := ExecLine(8, strings.Repeat("x", MaxExecScript+1)); err == nil {
		t.Fatal("a script longer than the console's line takes was accepted")
	}
	out := "eth0 UP 10.50.0.10/24\n"
	f, ok := ParseFrame("@@dc-twin exec-done 7 rc=0 " + base64.StdEncoding.EncodeToString([]byte(out)))
	if !ok || f.Kind != "exec-done" || f.Fields["id"] != "7" || f.Fields["rc"] != "0" || f.Record != out {
		t.Fatalf("exec-done frame %+v, %v", f, ok)
	}
	if f, ok := ParseFrame("@@dc-twin exec-done 9 rc=1 "); !ok || f.Record != "" || f.Fields["rc"] != "1" {
		t.Fatalf("an exec-done with no output: %+v, %v", f, ok)
	}
	if _, ok := ParseFrame("@@dc-twin exec 7 aXA="); ok {
		t.Fatal("the twin's own exec line read as the agent's")
	}
	s := GuestAgent()
	for _, want := range []string{"\"@@dc-twin exec \"*)", "exec-done", "base64 -d", "head -c"} {
		if !strings.Contains(s, want) {
			t.Errorf("the guest agent has no %q", want)
		}
	}
}

// agentOn is an attached agent at the far end of a console: it answers each exec with what run
// returns.
func agentOn(t *testing.T, run func(script string) (string, int)) (*Adapter, *guest) {
	t.Helper()
	reg, err := compile.Registry()
	if err != nil {
		t.Fatal(err)
	}
	est, _, err := compile.LoadEstate("../../test/linuxnet/testdata/tap-vm-nos.yaml")
	if err != nil {
		t.Fatal(err)
	}
	res, err := compile.Compile(est, reg, compile.Options{HostArch: "x86_64"})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(res.Model, reg, "")
	if err != nil {
		t.Fatal(err)
	}
	twinEnd, guestEnd := net.Pipe()
	a := New()
	a.rt = &adapter.Runtime{Engine: eng, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	g := &guest{a: a, d: res.Model.DeviceBy["site-1/hall-a/r01/server-01"], conn: twinEnd, attached: true}
	a.guests, a.byPath[g.d.Path] = []*guest{g}, g
	go g.read(twinEnd)
	go func() {
		rd := bufio.NewReader(guestEnd)
		for {
			s, err := rd.ReadString('\n')
			if err != nil {
				return
			}
			rest, ok := strings.CutPrefix(strings.TrimSpace(s), Marker+"exec ")
			if !ok {
				continue
			}
			id, enc, _ := strings.Cut(rest, " ")
			script, _ := base64.StdEncoding.DecodeString(enc)
			out, rc := run(string(script))
			_, _ = guestEnd.Write([]byte(Marker + "exec-done " + id + " rc=" + strconv.Itoa(rc) + " " + base64.StdEncoding.EncodeToString([]byte(out)) + "\n"))
		}
	}()
	t.Cleanup(func() { twinEnd.Close(); guestEnd.Close() })
	return a, g
}

func TestExecRunsInTheGuest(t *testing.T) {
	a, _ := agentOn(t, func(script string) (string, int) {
		if script == "echo hi; exit 3" {
			return "hi\n", 3
		}
		return "", 127
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, rc, err := a.Exec(ctx, "site-1/hall-a/r01/server-01", "echo hi; exit 3")
	if err != nil || out != "hi\n" || rc != 3 {
		t.Fatalf("Exec = %q, %d, %v", out, rc, err)
	}
	if _, _, err := a.Exec(ctx, "site-1/hall-a/r02/server-01", "true"); err == nil {
		t.Error("an exec on a host with no agent succeeded")
	}
}

// An exec is refused while the agent is not attached, and one waiting when it detaches (the
// guest rebooted) fails rather than waiting for an answer that will not come.
func TestExecWithoutAnAgent(t *testing.T) {
	block := make(chan struct{})
	a, g := agentOn(t, func(string) (string, int) { <-block; return "", 0 })
	defer close(block)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, _, err := a.Exec(ctx, g.d.Path, "sleep 60"); done <- err }()
	time.Sleep(100 * time.Millisecond)
	g.detach("the guest's kernel is starting")
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "detached") {
			t.Errorf("a pending exec ended with %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a pending exec did not end when the agent detached")
	}
	if _, _, err := a.Exec(ctx, g.d.Path, "true"); err == nil || !strings.Contains(err.Error(), "not attached") {
		t.Errorf("an exec with the agent detached: %v", err)
	}
}
