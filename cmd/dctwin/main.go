// Command dctwin runs and drives a datacenter twin (docs/spec/09-runtime.md — CLI).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/adapter/vm"
	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/control"
	"github.com/BudEcosystem/dc-twin/internal/report"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

const usage = `dctwin — a datacenter twin

  dctwin validate <estate.yaml>                 compile and validate; print the model's shape
  dctwin check --stack <name> <estate.yaml>     capability verdicts for a stack (06)
  dctwin up <estate.yaml> [--state DIR] [--listen ADDR] [--adapters a,b] [--console DIR]
  dctwin status | down | report [--json] | endpoints | domains | ui
  dctwin get <selector> [--json]
  dctwin events [--since N] [--select S] [--kind K] [--follow] [--no-reads]
  dctwin power on|off|graceful|restart|cycle|ac-cycle <selector>
  dctwin fault inject <selector> <fault> [--id ID] [--param k=v]
  dctwin fault clear <id|selector> [fault]
  dctwin fault list [selector]
  dctwin feed <name> up|down
  dctwin cable pull|replace <port-ref|link-id>
  dctwin chaos <rate-per-device-hour> <fault>...
  dctwin clock advance <duration>
  dctwin scenario run <scenario.yaml>
  dctwin credentials [selector]
  dctwin inspect <device> [redfish path]
  dctwin console <device> [--follow]            a vm device's serial console (a Serial over LAN stand-in)
  dctwin assets fetch

The running estate is found from --state, $DCTWIN_STATE, or ~/.dc-twin/current.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "validate":
		err = validate(args)
	case "check":
		err = check(args)
	case "up":
		err = up(args)
	case "assets":
		if len(args) > 0 && args[0] == "fetch" {
			var reg = mustRegistry()
			err = twin.FetchAssets(reg, os.Stdout)
			if err == nil {
				// the os: twin guest image vm devices boot (pinned, verified by digest)
				err = vm.FetchGuest(twin.AssetsDir(), os.Stdout)
			}
		} else {
			err = fmt.Errorf("assets fetch")
		}
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		err = client(cmd, args)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "dctwin:", err)
		os.Exit(1)
	}
}

func mustRegistry() *registryT {
	reg, err := compile.Registry()
	if err != nil {
		fmt.Fprintln(os.Stderr, "dctwin:", err)
		os.Exit(1)
	}
	return reg
}

func validate(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("validate <estate.yaml>")
	}
	reg := mustRegistry()
	est, _, err := compile.LoadEstate(args[0])
	if err != nil {
		return err
	}
	res, err := compile.Compile(est, reg, compile.Options{HostArch: compile.HostArch()})
	if err != nil {
		return err
	}
	m := res.Model
	byClass, byFid := map[string]int{}, map[string]int{}
	for _, d := range m.Devices {
		byClass[d.Class]++
		byFid[d.Fidelity]++
	}
	fmt.Printf("estate %s: valid\n  %d racks, %d devices, %d ports, %d links, %d domains\n", m.Name, len(m.Racks), len(m.Devices), len(m.Ports), len(m.Links), len(m.Domains))
	fmt.Printf("  by class: %s\n  by fidelity: %s\n  clock: %s\n", kv(byClass), kv(byFid), m.Clock)
	for _, l := range res.Lock.Packs {
		fmt.Printf("  pack %s@%s %s\n", l.Name, l.Version, l.Digest[:19])
	}
	for _, w := range res.Warnings {
		fmt.Println("  warning:", w)
	}
	return nil
}

func kv(m map[string]int) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var out []string
	for _, k := range ks {
		out = append(out, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(out, " ")
}

func up(args []string) error {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	state := fs.String("state", "", "state directory")
	listen := fs.String("listen", "127.0.0.1:0", "control API address (loopback)")
	adapters := fs.String("adapters", "", "comma-separated adapters (default: all)")
	consoleDir := fs.String("console", "", "serve a console bundle from this directory")
	allowTCG := fs.Bool("allow-tcg", false, "accept vm guests of another architecture under TCG")
	gateway := fs.String("gateway", "", "site gateway address (default: an ephemeral loopback port)")
	var file string
	var rest []string
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") && file == "" {
			file = args[i]
			continue
		}
		rest = append(rest, args[i])
	}
	fs.Parse(rest)
	if file == "" {
		return fmt.Errorf("up <estate.yaml>")
	}
	est, _, err := compile.LoadEstate(file)
	if err != nil {
		return err
	}
	explicitState := *state != ""
	if *state == "" {
		home, _ := os.UserHomeDir()
		*state = filepath.Join(home, ".dc-twin", est.Metadata.Name)
	}
	var names []string
	if *adapters != "" {
		names = strings.Split(*adapters, ",")
	}
	t, err := twin.Up(file, twin.Options{StateDir: *state, Adapters: names, AllowTCG: *allowTCG, GatewayAddr: *gateway})
	if err != nil {
		return err
	}
	var static http.Handler
	if *consoleDir != "" {
		static = http.FileServer(http.Dir(*consoleDir))
	}
	srv, err := control.Serve(t, *listen, static)
	if err != nil {
		t.Down()
		return err
	}
	// an estate started with an explicit --state is addressed by it (or DCTWIN_STATE); only the
	// default location becomes ~/.dc-twin/current, so a second estate does not take it over
	if !explicitState {
		home, _ := os.UserHomeDir()
		_ = os.MkdirAll(filepath.Join(home, ".dc-twin"), 0o700)
		_ = os.WriteFile(filepath.Join(home, ".dc-twin", "current"), []byte(*state), 0o600)
	}
	st := t.Engine.Status()
	var an []string
	for _, a := range t.Adapters {
		an = append(an, a.Name())
	}
	fmt.Printf("estate %s up: %v devices, %v links, clock %s, time scale %v\n", est.Metadata.Name, st["devices"], st["links"], st["clock"], st["timeScale"])
	fmt.Printf("  adapters: %s\n  control API: http://%s (token in %s/control.json)\n  site gateway (HTTP CONNECT): %s\n  state: %s\n",
		strings.Join(an, ", "), srv.Info.Addr, *state, t.Net.Gateway, *state)
	for _, w := range t.Warnings {
		fmt.Println("  warning:", w)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
	case <-srv.Down:
	}
	fmt.Println("estate going down")
	srv.Close()
	t.Down()
	return nil
}

// ---------------------------------------------------------------- client verbs

type api struct {
	info control.Info
}

func stateDir(args []string) (string, []string) {
	var rest []string
	dir := os.Getenv("DCTWIN_STATE")
	for i := 0; i < len(args); i++ {
		if args[i] == "--state" && i+1 < len(args) {
			dir = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	if dir == "" {
		home, _ := os.UserHomeDir()
		b, _ := os.ReadFile(filepath.Join(home, ".dc-twin", "current"))
		dir = strings.TrimSpace(string(b))
	}
	return dir, rest
}

func connect(dir string) (*api, error) {
	if dir == "" {
		return nil, fmt.Errorf("no running estate (start one with `dctwin up`)")
	}
	b, err := os.ReadFile(filepath.Join(dir, "control.json"))
	if err != nil {
		return nil, fmt.Errorf("no running estate at %s: %w", dir, err)
	}
	var a api
	if err := json.Unmarshal(b, &a.info); err != nil {
		return nil, err
	}
	return &a, nil
}

func (a *api) do(method, path string, body any, ctype string) ([]byte, error) {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		data, _ := json.Marshal(b)
		rd = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, "http://"+a.info.Addr+"/v1"+path, rd)
	req.Header.Set("Authorization", "Bearer "+a.info.Token)
	if ctype == "" {
		ctype = "application/json"
	}
	req.Header.Set("Content-Type", ctype)
	cl := &http.Client{Timeout: 2 * time.Hour}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var e map[string]string
		if json.Unmarshal(out, &e) == nil && e["error"] != "" {
			return nil, fmt.Errorf("%s", e["error"])
		}
		return nil, fmt.Errorf("%s: %s", resp.Status, out)
	}
	return out, nil
}

func flagVal(args []string, name string) (string, []string) {
	var rest []string
	val := ""
	for i := 0; i < len(args); i++ {
		if args[i] == name && i+1 < len(args) {
			val = args[i+1]
			i++
			continue
		}
		rest = append(rest, args[i])
	}
	return val, rest
}

func hasFlag(args []string, name string) (bool, []string) {
	var rest []string
	found := false
	for _, a := range args {
		if a == name {
			found = true
			continue
		}
		rest = append(rest, a)
	}
	return found, rest
}

func client(cmd string, args []string) error {
	dir, args := stateDir(args)
	a, err := connect(dir)
	if err != nil {
		return err
	}
	jsonOut, args := hasFlag(args, "--json")
	print := func(b []byte, err error) error {
		if err != nil {
			return err
		}
		os.Stdout.Write(b)
		return nil
	}
	switch cmd {
	case "status":
		b, err := a.do("GET", "/status", nil, "")
		if err != nil || jsonOut {
			return print(b, err)
		}
		var st map[string]any
		json.Unmarshal(b, &st)
		fmt.Printf("estate %v · clock %v · %v devices · %v links · seq %v · faults %v · twin incidents %v\n", st["estate"], st["clock"], st["devices"], st["links"], st["seq"], st["faults"], st["incidents"])
		fmt.Printf("  power: %v\n  health: %v\n  fidelity: %v\n  feeds: %v\n  adapters: %v\n", st["power"], st["health"], st["fidelity"], st["feeds"], st["adapters"])
		if doms, ok := st["domains"].(map[string]any); ok {
			var ks []string
			for k := range doms {
				ks = append(ks, k)
			}
			sort.Strings(ks)
			for _, k := range ks {
				if strings.Contains(k, "scale-up") {
					fmt.Printf("  domain %s: %v\n", k, doms[k])
				}
			}
		}
		return nil
	case "down":
		return print(a.do("POST", "/down", nil, ""))
	case "report":
		if jsonOut {
			return print(a.do("GET", "/report", nil, ""))
		}
		return print(a.do("GET", "/report?format=text", nil, ""))
	case "endpoints":
		return print(a.do("GET", "/endpoints", nil, ""))
	case "ui":
		fmt.Printf("http://%s/?token=%s\n", a.info.Addr, a.info.Token)
		return nil
	case "domains":
		return print(a.do("GET", "/domains", nil, ""))
	case "credentials":
		sel := ""
		if len(args) > 0 {
			sel = args[0]
		}
		return print(a.do("GET", "/credentials?select="+url.QueryEscape(sel), nil, ""))
	case "get":
		if len(args) < 1 {
			return fmt.Errorf("get <selector>")
		}
		b, err := a.do("GET", "/devices?select="+url.QueryEscape(args[0]), nil, "")
		if err != nil || jsonOut {
			return print(b, err)
		}
		var views []map[string]any
		json.Unmarshal(b, &views)
		for _, v := range views {
			id := v["identity"].(map[string]any)
			fmt.Printf("%-52s %-14s %-11s %-7s %-8s %-8v serial %-16v %v\n", v["path"], v["class"], v["power"], v["fidelity"], v["health"], v["lease"], id["serial"], faultsOf(v))
		}
		if len(views) == 1 {
			pretty, _ := json.MarshalIndent(views[0], "", "  ")
			fmt.Println(string(pretty))
		}
		return nil
	case "events":
		since, args := flagVal(args, "--since")
		sel, args := flagVal(args, "--select")
		kind, args := flagVal(args, "--kind")
		follow, args := hasFlag(args, "--follow")
		noReads, _ := hasFlag(args, "--no-reads")
		q := url.Values{}
		q.Set("since", since)
		q.Set("select", sel)
		q.Set("kind", kind)
		if noReads {
			q.Set("reads", "false")
		}
		if follow {
			req, _ := http.NewRequest("GET", "http://"+a.info.Addr+"/v1/events/stream?"+q.Encode(), nil)
			req.Header.Set("Authorization", "Bearer "+a.info.Token)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			buf := make([]byte, 64<<10)
			for {
				n, err := resp.Body.Read(buf)
				for _, line := range strings.Split(string(buf[:n]), "\n") {
					if strings.HasPrefix(line, "data: ") {
						var ev schema.Event
						json.Unmarshal([]byte(line[6:]), &ev)
						fmt.Println(formatEvent(ev))
					}
				}
				if err != nil {
					return nil
				}
			}
		}
		b, err := a.do("GET", "/events?"+q.Encode(), nil, "")
		if err != nil || jsonOut {
			return print(b, err)
		}
		var evs []schema.Event
		json.Unmarshal(b, &evs)
		for _, ev := range evs {
			fmt.Println(formatEvent(ev))
		}
		return nil
	case "power":
		if len(args) != 2 {
			return fmt.Errorf("power on|off|graceful|restart|cycle|ac-cycle <selector>")
		}
		return print(a.do("POST", "/power", map[string]any{"action": args[0], "select": args[1]}, ""))
	case "fault":
		if len(args) < 1 {
			return fmt.Errorf("fault inject|clear|list")
		}
		switch args[0] {
		case "inject":
			id, rest := flagVal(args[1:], "--id")
			params := map[string]any{}
			for {
				p, r2 := flagVal(rest, "--param")
				if p == "" {
					break
				}
				rest = r2
				k, v, _ := strings.Cut(p, "=")
				params[k] = v
			}
			if len(rest) != 2 {
				return fmt.Errorf("fault inject <selector> <fault>")
			}
			return print(a.do("POST", "/faults", map[string]any{"select": rest[0], "fault": rest[1], "id": id, "params": params}, ""))
		case "clear":
			if len(args) < 2 {
				return fmt.Errorf("fault clear <id|selector> [fault]")
			}
			f := ""
			if len(args) > 2 {
				f = args[2]
			}
			return print(a.do("POST", "/faults/clear", map[string]any{"target": args[1], "fault": f}, ""))
		case "list":
			sel := ""
			if len(args) > 1 {
				sel = args[1]
			}
			return print(a.do("GET", "/faults?select="+url.QueryEscape(sel), nil, ""))
		}
		return fmt.Errorf("fault inject|clear|list")
	case "feed":
		if len(args) != 2 {
			return fmt.Errorf("feed <name> up|down")
		}
		return print(a.do("POST", "/feeds", map[string]any{"feed": args[0], "up": args[1] == "up"}, ""))
	case "cable":
		if len(args) != 2 {
			return fmt.Errorf("cable pull|replace <port-ref|link-id>")
		}
		return print(a.do("POST", "/cable", map[string]any{"state": args[0], "link": args[1]}, ""))
	case "chaos":
		if len(args) < 1 {
			return fmt.Errorf("chaos <rate> <fault>...")
		}
		var rate float64
		fmt.Sscanf(args[0], "%g", &rate)
		return print(a.do("POST", "/chaos", map[string]any{"rate": rate, "faults": args[1:]}, ""))
	case "clock":
		if len(args) != 2 || args[0] != "advance" {
			return fmt.Errorf("clock advance <duration>")
		}
		return print(a.do("POST", "/clock/advance", map[string]any{"duration": args[1]}, ""))
	case "inspect":
		if len(args) < 1 {
			return fmt.Errorf("inspect <device> [path]")
		}
		p := "/redfish/v1"
		if len(args) > 1 {
			p = args[1]
		}
		return print(a.do("GET", "/inspect/redfish?device="+url.QueryEscape(args[0])+"&path="+url.QueryEscape(p), nil, ""))
	case "console":
		follow, args := hasFlag(args, "--follow")
		if len(args) != 1 {
			return fmt.Errorf("console <device> [--follow]")
		}
		var from int
		for {
			b, err := a.do("GET", fmt.Sprintf("/console?device=%s&from=%d", url.QueryEscape(args[0]), from), nil, "")
			if err != nil {
				return err
			}
			os.Stdout.Write(b)
			from += len(b)
			if !follow {
				return nil
			}
			time.Sleep(500 * time.Millisecond)
		}
	case "scenario":
		if len(args) != 2 || args[0] != "run" {
			return fmt.Errorf("scenario run <scenario.yaml>")
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		b, err := a.do("POST", "/scenarios/run", data, "application/yaml")
		if err != nil || jsonOut {
			return print(b, err)
		}
		return printResult(b)
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}

func faultsOf(v map[string]any) string {
	fs, _ := v["faults"].([]any)
	if len(fs) == 0 {
		return ""
	}
	var out []string
	for _, f := range fs {
		m := f.(map[string]any)
		out = append(out, fmt.Sprint(m["fault"]))
	}
	return "faults: " + strings.Join(out, ",")
}

func formatEvent(ev schema.Event) string {
	what := ev.Message
	if ev.Request != nil {
		what = fmt.Sprintf("%s %s %s %s", ev.Request.Canonical, ev.Request.Protocol, ev.Request.Method, ev.Request.Target)
	}
	if ev.State != nil {
		what = fmt.Sprintf("%s: %v → %v %s", ev.State.Field, ev.State.From, ev.State.To, ev.Message)
	}
	cause := ""
	if ev.Cause != 0 {
		cause = fmt.Sprintf(" ←#%d", ev.Cause)
	}
	t := ev.T
	if len(t) > 23 {
		t = t[11:23]
	}
	return fmt.Sprintf("#%-6d %s %-7s %-10s %s  %s%s", ev.Seq, t, ev.Kind, ev.Source.Class, ev.Path, what, cause)
}

func printResult(b []byte) error {
	var res struct {
		Scenario, Estate, Verdict, Reason, Took string
		Steps                                   []struct {
			Index        int
			Kind, Status string
			Detail       []string
			Took         string
		}
		Report *report.Report
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return err
	}
	fmt.Printf("scenario %s @ %s\n", res.Scenario, res.Estate)
	for _, s := range res.Steps {
		fmt.Printf("  %2d %-6s %-7s %8s  %s\n", s.Index, s.Kind, s.Status, s.Took, strings.Join(s.Detail, "; "))
	}
	fmt.Printf("result: %s (%s) %s\n", strings.ToUpper(res.Verdict), res.Took, res.Reason)
	if res.Report != nil {
		fmt.Print(res.Report.Text())
	}
	return nil
}
