package onie

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/BudEcosystem/dc-twin/internal/nos"
)

const usage = `usage: onie-discover [install|status] [flags]

  install   run ONIE's discovery on the management port until a NOS installs, then exit 0
            (the default)
  status    print the status file

flags:
`

// Main is the program's entry point (cmd/dctwin-day0), run as `onie-discover install` by a
// factory switch's start script before its NOS (18 — ONIE).
func Main(args []string) int {
	return run(args, os.Stdout, os.Stderr)
}

func run(args []string, stdout, stderr io.Writer) int {
	cmd := "install"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("onie-discover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage); fs.PrintDefaults() }
	e := &Env{Out: stdout}
	fs.StringVar(&e.Dir, "dir", nos.Day0Dir, "where the twin's inputs are (identity.json, images.json, factory.json)")
	fs.StringVar(&e.State, "state", filepath.Join(nos.Day0State, "onie"), "where status.json is written")
	fs.StringVar(&e.Iface, "iface", nos.MgmtIf, "the management port")
	fs.BoolVar(&e.RejectAll, "reject-all", envTrue("DCTWIN_ONIE_REJECT_ALL"), "every installer fails its checks (the onie-image-mismatch fault; also DCTWIN_ONIE_REJECT_ALL=1 or <dir>/"+RejectFile+")")
	fs.DurationVar(&e.Retry, "retry", RetryDelay, "the delay between waterfall passes")
	fs.StringVar(&e.Version, "onie-version", DefaultVersion, "the ONIE version reported in ONIE-VERSION and the User-Agent")
	fs.StringVar(&e.StaticURL, "url", "", "an installer URL tried before the waterfall (onie-install -i's, from the NOS)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch cmd {
	case "status":
		b, err := os.ReadFile(filepath.Join(e.State, "status.json"))
		if err != nil {
			fmt.Fprintf(stderr, "onie: %v\n", err)
			return 1
		}
		stdout.Write(b)
		return 0
	case "install":
	default:
		fs.Usage()
		return 2
	}
	dhcp, nc, err := System(e.Iface)
	if err != nil {
		fmt.Fprintf(stderr, "onie: %v\n", err)
		return 1
	}
	e.DHCP, e.Net, e.TFTP = dhcp, nc, TFTPGet
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := e.Install(ctx); err != nil {
		fmt.Fprintf(stderr, "onie: %v\n", err)
		return 1
	}
	return 0
}

func envTrue(name string) bool {
	switch strings.ToLower(os.Getenv(name)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
