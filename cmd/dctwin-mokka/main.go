// Command dctwin-mokka drives NVIDIA's Mokka (the nvml-mock driver tree) on container hosts from
// a dc-twin estate (internal/mokka, D49): it renders each host's GPU profile from the model, and
// keeps each node's runtime override document equal to the running twin's model.
//
//	dctwin-mokka render <estate.yaml> --select SEL --base DIR --out DIR
//	    one nvml-mock profile per selected host (<out>/<node>.yaml, given to the nvml-mock chart as
//	    gpu.customConfig), and <out>/hosts.json: each host's GPUs as the twin derives them and the
//	    GPU Feature Discovery labels the rendered profile implies. DIR holds Mokka's own profiles
//	    (the chart's profiles/ directory at the pinned release), the bases the twin's identity is
//	    written over.
//	dctwin-mokka sync <estate.yaml> --select SEL --control STATE/control.json [--once] -- CMD [ARG…]
//	    follow the running twin's control API and, whenever a host's accelerators change, run CMD
//	    with {node} in its arguments replaced by the host's node name and the host's override
//	    document on its standard input. The estate must be the one the twin runs: each host's
//	    system UUID is checked against the twin's.
//	dctwin-mokka overrides <estate.yaml> --select SEL --control STATE/control.json
//	    print each host's override document now.
//
// It is a separate command, not a dctwin adapter, so that it runs beside an unmodified twin and
// only where container hosts exist.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/compile"
	"github.com/BudEcosystem/dc-twin/internal/mokka"
	"github.com/BudEcosystem/dc-twin/internal/pack"
	"github.com/BudEcosystem/dc-twin/internal/selector"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprint(os.Stderr, "usage: dctwin-mokka render|sync|overrides <estate.yaml> [flags]  (see the package doc)\n")
		os.Exit(2)
	}
	cmd, estate, args := os.Args[1], os.Args[2], os.Args[3:]
	var err error
	switch cmd {
	case "render":
		err = render(estate, args)
	case "sync", "overrides":
		err = follow(cmd, estate, args)
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "dctwin-mokka:", err)
		os.Exit(1)
	}
}

// hosts compiles the estate and returns the selected hosts with NVIDIA GPUs.
func hosts(estate, sel string) ([]mokka.Host, *pack.Registry, error) {
	reg, err := compile.Registry()
	if err != nil {
		return nil, nil, err
	}
	est, _, err := compile.LoadEstate(estate)
	if err != nil {
		return nil, nil, err
	}
	res, err := compile.Compile(est, reg, compile.Options{HostArch: compile.HostArch()})
	if err != nil {
		return nil, nil, err
	}
	s, err := selector.Parse(sel)
	if err != nil {
		return nil, nil, err
	}
	var out []mokka.Host
	for _, d := range res.Model.Devices {
		if !d.IsHost() || !s.MatchPath(d.Path) || !s.MatchAttrs(d.Attr) {
			continue
		}
		if h, ok := mokka.HostOf(res.Model, d); ok {
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("%q selects no host with NVIDIA GPUs", sel)
	}
	return out, reg, nil
}

type hostJSON struct {
	mokka.Host
	Base   string            `json:"base"`
	Labels map[string]string `json:"gfdLabels"`
	Notes  []string          `json:"notes"`
	File   string            `json:"profile"`
}

func render(estate string, args []string) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	sel := fs.String("select", "", "hosts to render (a dc-twin selector)")
	base := fs.String("base", "", "directory of Mokka's profiles (<name>.yaml)")
	out := fs.String("out", ".", "output directory")
	_ = fs.Parse(args)
	if *sel == "" || *base == "" {
		return fmt.Errorf("render needs --select and --base")
	}
	hs, _, err := hosts(estate, *sel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	var all []hostJSON
	for _, h := range hs {
		name := mokka.BaseFor(h.GPUs[0].Name)
		if name == "" {
			return fmt.Errorf("%s: Mokka ships no profile for %q", h.Path, h.GPUs[0].Name)
		}
		b, err := os.ReadFile(filepath.Join(*base, name+".yaml"))
		if err != nil {
			return err
		}
		p, err := mokka.Render(h, name, b)
		if err != nil {
			return err
		}
		file := filepath.Join(*out, h.Node+".yaml")
		if err := os.WriteFile(file, p.Config, 0o644); err != nil {
			return err
		}
		all = append(all, hostJSON{Host: h, Base: name, Labels: mokka.Labels(p), Notes: p.Notes, File: file})
		fmt.Printf("%s\t%s\t%d × %s over mokka/%s\n", h.Node, h.Path, len(h.GPUs), h.GPUs[0].Name, name)
	}
	b, _ := json.MarshalIndent(all, "", "  ")
	return os.WriteFile(filepath.Join(*out, "hosts.json"), b, 0o644)
}

func follow(cmd, estate string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	sel := fs.String("select", "", "hosts to drive (a dc-twin selector)")
	ctl := fs.String("control", "", "the running twin's control.json (its state directory)")
	once := fs.Bool("once", false, "apply every host's document once and exit")
	resync := fs.Duration("resync", 10*time.Second, "re-render every host this often regardless of events")
	_ = fs.Parse(args)
	if *sel == "" || *ctl == "" {
		return fmt.Errorf("%s needs --select and --control", cmd)
	}
	hs, reg, err := hosts(estate, *sel)
	if err != nil {
		return err
	}
	addr, token, err := mokka.ReadControl(*ctl)
	if err != nil {
		return err
	}
	src := mokka.ControlSource{Addr: addr, Token: token, Reg: reg}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for _, h := range hs {
		if err := src.Check(ctx, h); err != nil {
			return err
		}
	}
	if cmd == "overrides" {
		for _, h := range hs {
			gs, err := src.GPUs(ctx, h)
			if err != nil {
				return err
			}
			fmt.Printf("--- # node %s (%s)\n%s", h.Node, h.Path, mokka.Overrides(gs))
		}
		return nil
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("sync needs a command after --: it receives each document on stdin")
	}
	s := &mokka.Syncer{Source: src, Sink: mokka.ExecSink{Argv: fs.Args()}, Hosts: hs, Resync: *resync,
		Applied: func(node string, doc []byte) {
			fmt.Printf("%s applied to %s\n%s", time.Now().UTC().Format(time.RFC3339Nano), node, doc)
		}}
	if *once {
		return s.SyncOnce(ctx, true)
	}
	fmt.Printf("following %s for %d hosts\n", addr, len(hs))
	s.Run(ctx, func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) })
	return nil
}
