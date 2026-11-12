// Package nvcli is the twin's `nv`, Cumulus Linux's NVUE command line (D76; 18 — The first
// push), for a generic/nvue-frr switch in factory state: the commands a day-0 tool or its ZTP
// script runs over SSH or as root, on the same emulated NVUE tree the REST API serves
// (internal/nos/nvue). It is a client of that NVUE, at https://127.0.0.1:8765/nvue_v1 in the
// switch, reaching it by the local trust path (nvue.SetLocalToken) as the user who ran it, as
// Cumulus's `nv` reaches NVUE locally without a password.
//
// What it does, after NVIDIA's NVUE CLI and reference pages (docs/reviews/2026-11-11-day0-vendor-
// facts.md C.2):
//
//	nv set <path> <value>       stage a change in the user's pending revision
//	nv unset <path>             stage a removal in it
//	nv show [<path>] [-o json|yaml|auto] [--rev applied|startup|pending|<n>] [--pending|--applied|--operational]
//	nv config apply [--y|-y|--assume-yes|--assume-no] [<rev>|applied|startup|empty]
//	nv config diff [<base> [<target>]] [-o yaml|json|commands]
//	nv config replace <file>    the pending revision becomes the file (startup.yaml's form, JSON, or nv set/unset lines)
//	nv config patch <file>      the file merged into the pending revision
//	nv config save              the applied configuration becomes the startup one
//	nv config show [-o yaml|json|commands] [-r <rev>]
//	nv config history [<rev>]
//	nv config detach
//	nv config revision
//
// Interface names take NVUE's ranges and lists (swp1-4, swp1s0-3, swp1,swp3). `-o json` prints
// NVUE's JSON as the REST API has it; the default output (auto) is YAML of the same tree, not
// NVUE's operational/applied/pending table, which `nv show` prints on a switch: a script that
// parses `nv show` should use -o json or -o yaml, as NVIDIA advises. Any other command (`nv
// action`, `nv list-commands`) is refused by name.
package nvcli

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/nos/nvue"
)

// DefaultURL is NVUE's REST API in the switch, where `nv` reaches it.
const DefaultURL = "https://127.0.0.1:8765/nvue_v1"

// Main is `nv` (cmd/dctwin-day0): its arguments without the program's name. DCTWIN_NVUE_URL and
// DCTWIN_NVUE_TOKEN_FILE override where NVUE is and where its local token is read from.
func Main(args []string) int {
	base := os.Getenv("DCTWIN_NVUE_URL")
	if base == "" {
		base = DefaultURL
	}
	tokenFile := os.Getenv("DCTWIN_NVUE_TOKEN_FILE")
	if tokenFile == "" {
		tokenFile = nvue.TokenFile
	}
	c := &client{base: strings.TrimRight(base, "/"), user: invokingUser(), out: os.Stdout, errw: os.Stderr,
		http: &http.Client{Timeout: 6 * time.Minute, Transport: &http.Transport{
			// NVUE's certificate is the estate CA's, issued for the switch's name; this is the
			// switch talking to itself
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		}}}
	if b, err := os.ReadFile(tokenFile); err == nil {
		c.token = strings.TrimSpace(string(b))
	} else if len(args) > 0 && args[0] != "-h" && args[0] != "--help" && args[0] != "help" {
		fmt.Fprintf(os.Stderr, "Error: NVUE's local token (%s) cannot be read: %v\n", tokenFile, err)
		return 1
	}
	return c.run(args)
}

// invokingUser is who ran `nv`, as NVUE records it: the uid's name; under sudo (uid 0) the user
// sudo ran it for ($SUDO_USER), else $USER or $LOGNAME, else root. A non-root user's environment
// is not believed over its uid, as Cumulus's NVUE takes the user from the local socket.
func invokingUser() string {
	uid := os.Getuid()
	if uid != 0 {
		if n := passwdName(uid); n != "" {
			return n
		}
	}
	for _, v := range []string{"SUDO_USER", "USER", "LOGNAME"} {
		if u := os.Getenv(v); u != "" {
			return u
		}
	}
	if n := passwdName(uid); n != "" {
		return n
	}
	return "root"
}

func passwdName(uid int) string {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		p := strings.Split(sc.Text(), ":")
		if len(p) > 2 && p[2] == strconv.Itoa(uid) {
			return p[0]
		}
	}
	return ""
}

// client is one `nv` run.
type client struct {
	base, user, token string
	http              *http.Client
	out, errw         io.Writer
}

const usage = `usage: nv <command> [<args>]

  set <path> <value>          stage a change in your pending revision
  unset <path>                stage a removal in it
  show [<path>] [-o json|yaml|auto] [--rev applied|startup|pending|<n>]
  config apply [--y] [<rev>|applied|startup|empty]
  config diff [<base> [<target>]] [-o yaml|json|commands]
  config replace <file>       make the pending revision the file (YAML, JSON, or nv set/unset lines)
  config patch <file>         merge the file into the pending revision
  config save                 make the applied configuration the startup one
  config show [-o yaml|json|commands] [-r <rev>]
  config history [<rev>]
  config detach
  config revision

The twin's nv (dc-twin, D76): Cumulus Linux's NVUE CLI over the twin's emulated NVUE.
`
