package dhcp

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// The iPXE subset the stacks emit (05 — The boot walk): set, clear, isset, iseq, goto and
// :labels, chain, kernel, initrd, imgfetch (and module, imgselect, imgexec), boot; echo, sleep
// and the network commands are accepted and do nothing, because the walker has already done
// what they would. Semantics follow iPXE's shell: `a || b` runs b when a fails, `a && b` when
// it succeeds, a line whose last command fails ends the script (so the iPXE idiom
// `iseq … && goto x ||` is honoured), relative URLs resolve against the script's own URL, and
// ${name} and ${name:type} expand from the host's settings.

// ipxeFetch GETs a URL fully. It returns the body only when keep is set (scripts and chained
// images, which must be inspected), and always the size transferred.
type ipxeFetch func(ctx context.Context, u *url.URL, keep bool) (body []byte, n int64, err error)

type artifact struct {
	Kind  string // kernel, initrd, imgfetch, chain
	URL   string
	Bytes int64
}

// ipxeResult is where a script run ended.
type ipxeResult struct {
	Booted    bool
	Image     string // what boot executes
	Step      string // the walk step it stopped at when not booted: script or artifacts
	Reason    string
	Artifacts []artifact
	Scripts   []string
}

type ipxeRun struct {
	vars  map[string]string
	fetch ipxeFetch
	res   ipxeResult
	image string
	depth int
	ops   int
}

const maxIPXEOps = 100000

func isIPXEScript(b []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(b, " \t\r\n\ufeff"), []byte("#!ipxe"))
}

// runIPXE interprets a script fetched from base.
func runIPXE(ctx context.Context, script []byte, base *url.URL, vars map[string]string, fetch ipxeFetch) ipxeResult {
	r := &ipxeRun{vars: vars, fetch: fetch}
	r.script(ctx, string(script), base)
	return r.res
}

// stop records where the run ended without booting.
type stopErr struct{ step, reason string }

func (e *stopErr) Error() string { return e.reason }

func (r *ipxeRun) script(ctx context.Context, src string, base *url.URL) {
	r.res.Scripts = append(r.res.Scripts, base.String())
	r.depth++
	if r.depth > 8 {
		r.res.Step, r.res.Reason = "script", "chain depth above 8"
		return
	}
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	labels := map[string]int{}
	for i, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, ":") {
			labels[strings.TrimSpace(t[1:])] = i
		}
	}
	for pc := 0; pc < len(lines); pc++ {
		if r.ops++; r.ops > maxIPXEOps {
			r.res.Step, r.res.Reason = "script", "the script did not terminate"
			return
		}
		if ctx.Err() != nil {
			r.res.Step, r.res.Reason = "script", ctx.Err().Error()
			return
		}
		line := strings.TrimSpace(lines[pc])
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ":") {
			continue
		}
		ok, jump, done, err := r.line(ctx, line, base)
		if done || r.res.Booted || r.res.Reason != "" {
			return
		}
		if err != nil {
			var se *stopErr
			step, reason := "script", err.Error()
			if asStop(err, &se) {
				step, reason = se.step, se.reason
			}
			r.res.Step, r.res.Reason = step, reason
			return
		}
		if jump != "" {
			to, found := labels[jump]
			if !found {
				r.res.Step, r.res.Reason = "script", fmt.Sprintf("goto %s: no such label", jump)
				return
			}
			pc = to
			continue
		}
		if !ok {
			r.res.Step, r.res.Reason = "script", fmt.Sprintf("%q failed, and iPXE ends a script at an unhandled failure", line)
			return
		}
	}
	r.res.Step, r.res.Reason = "script", "the script "+baseName(base.String())+" ended without booting (iPXE returns to the firmware)"
}

func asStop(err error, se **stopErr) bool {
	s, ok := err.(*stopErr)
	if ok {
		*se = s
	}
	return ok
}

// line runs one line's command chain. ok is the chain's status; jump a goto target; done means
// a chained script took over and has finished the run.
func (r *ipxeRun) line(ctx context.Context, line string, base *url.URL) (ok bool, jump string, done bool, err error) {
	toks := tokens(line)
	var cmds [][]string
	var ops []string
	cur := []string{}
	for _, t := range toks {
		if t == "||" || t == "&&" {
			cmds = append(cmds, cur)
			ops = append(ops, t)
			cur = []string{}
			continue
		}
		cur = append(cur, t)
	}
	cmds = append(cmds, cur)
	status := true
	for i, c := range cmds {
		if i > 0 {
			if ops[i-1] == "||" && status {
				continue
			}
			if ops[i-1] == "&&" && !status {
				continue
			}
		}
		if len(c) == 0 { // `cmd ||` with nothing after it: success
			status = true
			continue
		}
		st, j, d, e := r.command(ctx, c, base)
		if e != nil || d {
			return false, "", d, e
		}
		if j != "" {
			return true, j, false, nil
		}
		status = st
	}
	return status, "", false, nil
}

// tokens splits a command line on whitespace, honouring double and single quotes.
func tokens(s string) []string {
	var out []string
	var b strings.Builder
	in := rune(0)
	has := false
	for _, c := range s {
		switch {
		case in != 0 && c == in:
			in = 0
		case in == 0 && (c == '"' || c == '\''):
			in, has = c, true
		case in == 0 && (c == ' ' || c == '\t'):
			if has || b.Len() > 0 {
				out = append(out, b.String())
				b.Reset()
				has = false
			}
		default:
			b.WriteRune(c)
		}
	}
	if has || b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}

// expand substitutes ${name} and ${name:type}.
func (r *ipxeRun) expand(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "${")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.Index(s[i:], "}")
		if j < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(r.setting(s[i+2 : i+j]))
		s = s[i+j+1:]
	}
}

func (r *ipxeRun) setting(ref string) string {
	name, typ, _ := strings.Cut(ref, ":")
	v, ok := r.vars[name]
	if !ok {
		// a scoped name (net0/mac) falls back to the global one and back again
		if _, short, scoped := strings.Cut(name, "/"); scoped {
			v = r.vars[short]
		} else {
			v = r.vars["net0/"+name]
		}
	}
	switch typ {
	case "hexhyp":
		return strings.ReplaceAll(v, ":", "-")
	case "hexraw":
		return strings.ReplaceAll(v, ":", "")
	case "uristring":
		return url.QueryEscape(v)
	}
	return v
}

// options strips iPXE command options (--name x, -n x, --autofree …) from args.
func options(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && len(out) == 0 {
			switch a {
			case "-n", "--name", "-t", "--timeout", "-k", "--key", "-d", "--default", "-m", "--menu":
				i++
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

func (r *ipxeRun) command(ctx context.Context, c []string, base *url.URL) (ok bool, jump string, done bool, err error) {
	args := make([]string, len(c))
	for i, t := range c {
		args[i] = r.expand(t)
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "set":
		if len(rest) == 0 {
			return false, "", false, nil
		}
		r.vars[rest[0]] = strings.Join(rest[1:], " ")
		return true, "", false, nil
	case "clear":
		if len(rest) > 0 {
			delete(r.vars, rest[0])
		}
		return true, "", false, nil
	case "isset":
		return len(rest) > 0 && rest[0] != "", "", false, nil
	case "iseq":
		if len(rest) < 2 {
			return len(rest) == 0 || rest[0] == "", "", false, nil
		}
		return rest[0] == rest[1], "", false, nil
	case "goto":
		if len(rest) == 0 {
			return false, "", false, nil
		}
		return true, rest[0], false, nil
	case "kernel", "initrd", "imgfetch", "module", "imgexec", "chain", "imgselect":
		pos := options(rest)
		if len(pos) == 0 {
			return false, "", false, nil
		}
		if cmd == "imgselect" {
			r.image = pos[0]
			return true, "", false, nil
		}
		u, perr := base.Parse(pos[0])
		if perr != nil {
			return false, "", false, &stopErr{"artifacts", fmt.Sprintf("%s %s: %v", cmd, pos[0], perr)}
		}
		keep := cmd == "chain" || cmd == "imgexec"
		body, n, ferr := r.fetch(ctx, u, keep)
		if ferr != nil {
			if se, isStop := ferr.(*stopErr); isStop {
				return false, "", false, se
			}
			// a failed download is a failed command; the script may handle it with ||
			r.res.Artifacts = append(r.res.Artifacts, artifact{Kind: cmd + " (failed: " + ferr.Error() + ")", URL: u.String()})
			return false, "", false, nil
		}
		r.res.Artifacts = append(r.res.Artifacts, artifact{Kind: cmd, URL: u.String(), Bytes: n})
		switch cmd {
		case "kernel":
			r.image = u.String()
		case "chain", "imgexec":
			if isIPXEScript(body) {
				r.script(ctx, string(body), u)
				return true, "", true, nil
			}
			r.image = u.String()
			r.res.Booted, r.res.Image = true, r.image
			return true, "", true, nil
		}
		return true, "", false, nil
	case "boot":
		if len(rest) > 0 {
			r.image = rest[0]
		}
		if r.image == "" {
			return false, "", false, nil // `boot` with nothing selected fails, as in iPXE
		}
		r.res.Booted, r.res.Image = true, r.image
		return true, "", true, nil
	case "echo", "sleep", "dhcp", "ifopen", "ifclose", "ifconf", "ifstat", "route", "sync", "ntp", "imgfree",
		"imgstat", "colour", "cpair", "console", "menu", "item", "imgverify", "imgtrust", "show", "cpuid":
		return true, "", false, nil
	case "prompt":
		for _, a := range rest {
			if a == "--timeout" || a == "-t" {
				return false, "", false, nil // nobody presses a key: the prompt times out
			}
		}
		return false, "", false, &stopErr{"script", "prompt waits for a key press with no timeout"}
	case "choose":
		def := ""
		for i := 0; i+1 < len(rest); i++ {
			if rest[i] == "--default" || rest[i] == "-d" {
				def = rest[i+1]
			}
		}
		pos := options(rest)
		if def == "" || len(pos) == 0 {
			return false, "", false, &stopErr{"script", "choose waits for a menu selection with no default"}
		}
		r.vars[pos[0]] = def
		return true, "", false, nil
	case "exit":
		return false, "", false, &stopErr{"script", "exit: iPXE returns to the firmware"}
	case "reboot", "poweroff":
		return false, "", false, &stopErr{"script", cmd + " requested by the script"}
	case "shell", "login", "config":
		return false, "", false, &stopErr{"script", cmd + " is interactive"}
	case "params", "param":
		return false, "", false, &stopErr{"script", "params/param (HTTP POST forms) are not implemented"}
	}
	return false, "", false, &stopErr{"script", fmt.Sprintf("unsupported iPXE command %q", cmd)}
}

// baseName is a URL's last path element, for messages.
func baseName(u string) string {
	if pu, err := url.Parse(u); err == nil && pu.Path != "" {
		return path.Base(pu.Path)
	}
	return u
}
