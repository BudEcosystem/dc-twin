package ztp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// sonic-ztp's built-in plugins (E.6, src/usr/lib/ztp/plugins/) and its url and dynamic-url
// objects (E.4, ZTPObjects.py), as the twin runs them.

// urlSpec is a url object: "source" (mandatory), "destination", "secure", "timeout" and
// "include-http-headers"; a plain string is its source. encrypted and curl-arguments are not
// modelled.
type urlSpec struct {
	source, destination string
	secure, headers     bool
	timeout             time.Duration
}

func parseURL(v any) (urlSpec, error) {
	s := urlSpec{secure: true, headers: true}
	switch v := v.(type) {
	case string:
		s.source = v
	case map[string]any:
		s.source = strOf(v, "source")
		s.destination = strOf(v, "destination")
		s.secure = boolOf(v, "secure", true)
		s.headers = boolOf(v, "include-http-headers", true)
		if t := intOf(v, "timeout", 0); t > 0 {
			s.timeout = time.Duration(t) * time.Second
		}
	default:
		return s, fmt.Errorf("url: want a string or an object, have %T", v)
	}
	if strings.TrimSpace(s.source) == "" {
		return s, errors.New(`url: no "source"`)
	}
	return s, nil
}

// dynamicURL is a dynamic-url object (E.4): its source is prefix + identifier + suffix, where
// identifier is hostname, hostname-fqdn, serial-number, product-name, sonic-version, any other
// literal string, or {"url": …}, a script whose output is the identifier. There is no mac
// identifier.
func (z *sonic) dynamicURL(ctx context.Context, v any) (urlSpec, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return urlSpec{}, fmt.Errorf("dynamic-url: want an object, have %T", v)
	}
	src, ok := m["source"].(map[string]any)
	if !ok {
		return urlSpec{}, errors.New(`dynamic-url: no "source" object`)
	}
	id, err := z.identifier(ctx, src["identifier"])
	if err != nil {
		return urlSpec{}, err
	}
	flat := map[string]any{}
	for k, v := range m {
		flat[k] = v
	}
	flat["source"] = strOf(src, "prefix") + id + strOf(src, "suffix")
	return parseURL(flat)
}

func (z *sonic) identifier(ctx context.Context, v any) (string, error) {
	switch v := v.(type) {
	case string:
		switch v {
		case "hostname":
			h, err := z.c.hostname()
			return strings.SplitN(h, ".", 2)[0], err
		case "hostname-fqdn":
			return z.c.hostname()
		case "serial-number":
			return z.id.Serial, nil
		case "product-name":
			return z.id.Model, nil
		case "sonic-version":
			return z.id.Version, nil
		}
		return v, nil
	case map[string]any:
		spec, err := parseURL(v["url"])
		if err != nil {
			return "", fmt.Errorf("identifier: %w", err)
		}
		body, err := z.download(ctx, spec)
		if err != nil {
			return "", fmt.Errorf("identifier: %w", err)
		}
		var out bytes.Buffer
		code, err := z.c.runScript(ctx, body, &out, nil)
		if err != nil || code != 0 {
			return "", fmt.Errorf("identifier: script %s exited %d: %v", spec.source, code, err)
		}
		return strings.TrimSpace(out.String()), nil
	case nil:
		return "", errors.New(`dynamic-url: no "identifier"`)
	}
	return "", fmt.Errorf("identifier: want a string or an object, have %T", v)
}

func (z *sonic) download(ctx context.Context, s urlSpec) ([]byte, error) {
	f := z.fetcher()
	t := s.timeout
	if t == 0 {
		t = f.timeout
	}
	return f.getWith(ctx, s.source, !s.secure, s.headers, t)
}

// source is a section's dynamic-url, else its url (dynamic-url takes precedence, E.4).
// Either counts only when it is set to something (sonic-ztp tests them with Python's truth:
// an empty string or object is no url).
func (z *sonic) source(ctx context.Context, m map[string]any) (urlSpec, bool, error) {
	if v := m["dynamic-url"]; truthy(v) {
		s, err := z.dynamicURL(ctx, v)
		return s, true, err
	}
	if v := m["url"]; truthy(v) {
		s, err := parseURL(v)
		return s, true, err
	}
	return urlSpec{}, false, nil
}

// truthy is a JSON value's truth as Python's, which sonic-ztp tests its objects with.
func truthy(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v != ""
	case map[string]any:
		return len(v) > 0
	case []any:
		return len(v) > 0
	}
	return true
}

// pluginOf is how sonic-ztp resolves a section's plugin (ZTPJson.plugin, ZTPSections.py): a
// "plugin" string names it; an object whose dynamic-url or url is set is a script to download
// (script); an object's "name" names it; otherwise, and without "plugin", it is the section's
// name with a leading NNN- stripped. A "plugin" of any other type is an error.
func pluginOf(name string, sec map[string]any) (builtin string, script map[string]any, err error) {
	builtin = leadingNum.ReplaceAllString(name, "")
	switch p := sec["plugin"].(type) {
	case string:
		builtin = p
	case map[string]any:
		if truthy(p["dynamic-url"]) || truthy(p["url"]) {
			return "", p, nil
		}
		if n, ok := p["name"].(string); ok {
			builtin = n
		}
	case nil:
	default:
		return "", nil, fmt.Errorf(`"plugin" is a %T`, p)
	}
	return builtin, nil, nil
}

// plugin runs a section by its plugin and returns the plugin's exit code. The plugin is
// "plugin" when the section has one: a name; or an object whose dynamic-url or url (in that
// order) is a script to run, else whose name is the built-in, with args given to the script and
// ignore-section-data keeping the section's input.json from it (E.5). Without "plugin" it is
// the section's name with a leading NNN- stripped.
func (z *sonic) plugin(ctx context.Context, name string, sec map[string]any) (int, error) {
	builtin, p, err := pluginOf(name, sec)
	if err != nil {
		return 1, err
	}
	if p != nil {
		spec, _, err := z.source(ctx, p)
		if err != nil {
			return 1, err
		}
		return z.script(ctx, name, sec, spec, p)
	}
	switch builtin {
	case "configdb-json":
		return z.configDB(ctx, name, sec)
	case "provisioning-script":
		spec, ok, err := z.source(ctx, sec)
		if err != nil {
			return 1, err
		}
		if !ok {
			return 1, errors.New(`provisioning-script: no "url" or "dynamic-url"`)
		}
		return z.script(ctx, name, sec, spec, nil)
	case "connectivity-check":
		return z.connectivity(ctx, name, sec)
	case "snmp":
		return z.snmp(ctx, name, sec)
	case "download":
		return z.downloadFiles(ctx, sec)
	case "firmware":
		return 1, errors.New("firmware: not run on the twin: an image install is modelled (D75); " +
			"the section is FAILED unless it has ignore-result")
	case "graphservice":
		return 1, errors.New("graphservice: refused by name: minigraph is not supported by the twin's ZTP; use configdb-json")
	}
	return 1, fmt.Errorf("no plugin %q", builtin)
}

// script runs a section's script plugin with the section's input.json as its argument (written
// to /var/lib/ztp/sections/<section>/input.json, E.2) unless ignore-section-data, then args.
func (z *sonic) script(ctx context.Context, name string, sec map[string]any, spec urlSpec, p map[string]any) (int, error) {
	body, err := z.download(ctx, spec)
	if err != nil {
		return 1, err
	}
	var args []string
	if !boolOf(p, "ignore-section-data", false) {
		in := filepath.Join(z.c.sectionsDir, name, "input.json")
		if err := writeJSON(in, map[string]any{name: sec}); err != nil {
			return 1, err
		}
		args = append(args, in)
	}
	switch a := p["args"].(type) {
	case string:
		args = append(args, strings.Fields(a)...)
	case []any:
		for _, v := range a {
			args = append(args, fmt.Sprint(v))
		}
	}
	z.log.printf("%s: running %s", name, spec.source)
	out := z.log.writer()
	defer out.Close()
	return z.c.runScript(ctx, body, out, nil, args...)
}

// configDB is configdb-json (E.6): the file at url or dynamic-url is downloaded to its
// destination (/etc/sonic/config_db.json by default) and applied with --reload-cmd
// (clear-config true, the default, as `config reload -y <file>`) or --load-cmd (clear-config
// false, `config load -y <file>`), then saved with --save-cmd when save-config is true.
// Unless overwrite-device-metadata is true, the switch's current DEVICE_METADATA|localhost
// (from --dump-metadata-cmd, sonic-cfggen -d --var-json DEVICE_METADATA) is merged over the
// downloaded file's before it is applied, as the plugin keeps the device's own metadata.
func (z *sonic) configDB(ctx context.Context, name string, sec map[string]any) (int, error) {
	spec, ok, err := z.source(ctx, sec)
	if err != nil {
		return 1, err
	}
	if !ok {
		return 1, errors.New(`configdb-json: no "url" or "dynamic-url"`)
	}
	body, err := z.download(ctx, spec)
	if err != nil {
		return 1, err
	}
	var db map[string]any
	if err := json.Unmarshal(body, &db); err != nil {
		return 1, fmt.Errorf("configdb-json: %s is not a config_db.json: %w", spec.source, err)
	}
	if !boolOf(sec, "overwrite-device-metadata", false) {
		if cur, err := z.deviceMetadata(ctx); err != nil {
			z.log.printf("%s: the device's DEVICE_METADATA is not kept: %v", name, err)
		} else if len(cur) > 0 {
			meta, _ := db["DEVICE_METADATA"].(map[string]any)
			if meta == nil {
				meta = map[string]any{}
			}
			local, _ := meta["localhost"].(map[string]any)
			if local == nil {
				local = map[string]any{}
			}
			for k, v := range cur {
				local[k] = v
			}
			meta["localhost"] = local
			db["DEVICE_METADATA"] = meta
			if body, err = json.MarshalIndent(db, "", "    "); err != nil {
				return 1, err
			}
		}
	}
	dest := spec.destination
	if dest == "" {
		dest = filepath.Join(z.c.sonicDir, "config_db.json")
	}
	if err := writeFileAtomic(dest, body, 0o644); err != nil {
		return 1, err
	}
	cmd := z.c.reloadCmd
	if !boolOf(sec, "clear-config", true) {
		cmd = z.c.loadCmd
	}
	out := z.log.writer()
	defer out.Close()
	z.log.printf("%s: applying %s: %s", name, dest, cmd)
	code, err := z.c.shell(ctx, cmd, out, dest)
	if err != nil || code != 0 {
		return code, err
	}
	if boolOf(sec, "save-config", false) {
		z.log.printf("%s: saving: %s", name, z.c.saveCmd)
		return z.c.shell(ctx, z.c.saveCmd, out)
	}
	return 0, nil
}

// deviceMetadata is the switch's DEVICE_METADATA|localhost, from --dump-metadata-cmd (its
// output a DEVICE_METADATA table: {"localhost": {...}}); empty when the command is.
func (z *sonic) deviceMetadata(ctx context.Context) (map[string]any, error) {
	if strings.TrimSpace(z.c.dumpMetaCmd) == "" {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", z.c.dumpMetaCmd)
	cmd.Env = z.c.env()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %v %s", z.c.dumpMetaCmd, err, strings.TrimSpace(stderr.String()))
	}
	var t map[string]any
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("%s: %w", z.c.dumpMetaCmd, err)
	}
	local, _ := t["localhost"].(map[string]any)
	return local, nil
}

func strList(v any) []string {
	switch v := v.(type) {
	case string:
		return strings.Fields(strings.ReplaceAll(v, ",", " "))
	case []any:
		var out []string
		for _, x := range v {
			out = append(out, fmt.Sprint(x))
		}
		return out
	}
	return nil
}

// connectivity is connectivity-check (E.6): every host of ping-hosts (and ping6-hosts) must
// answer `ping -c <ping-count> -W <timeout>` (with -w <deadline> when given) within retry-count
// tries retry-interval seconds apart (defaults 12, 5, 3 and 1 s). Pings leave through VRF mgmt
// (`ip vrf exec mgmt`) when the switch has it and has ip.
func (z *sonic) connectivity(ctx context.Context, name string, sec map[string]any) (int, error) {
	retries := intOf(sec, "retry-count", 12)
	interval := time.Duration(intOf(sec, "retry-interval", 5)) * time.Second
	count := intOf(sec, "ping-count", 3)
	timeout := intOf(sec, "timeout", 1)
	deadline := intOf(sec, "deadline", 0)
	type host struct {
		addr string
		v6   bool
	}
	var hosts []host
	for _, h := range strList(sec["ping-hosts"]) {
		hosts = append(hosts, host{h, false})
	}
	for _, h := range strList(sec["ping6-hosts"]) {
		hosts = append(hosts, host{h, true})
	}
	if len(hosts) == 0 {
		return 1, errors.New(`connectivity-check: no "ping-hosts" or "ping6-hosts"`)
	}
	out := z.log.writer()
	defer out.Close()
	for _, h := range hosts {
		argv := []string{z.c.pingCmd}
		if h.v6 {
			argv = append(argv, "-6")
		}
		argv = append(argv, "-c", fmt.Sprint(count), "-W", fmt.Sprint(timeout))
		if deadline > 0 {
			argv = append(argv, "-w", fmt.Sprint(deadline))
		}
		argv = append(argv, h.addr)
		if z.c.scriptVRF == "" && z.c.vrf != "" && z.c.vrfExists != nil && z.c.vrfExists(z.c.vrf) {
			if _, err := exec.LookPath(z.c.ipCmd); err == nil {
				argv = append([]string{z.c.ipCmd, "vrf", "exec", z.c.vrf}, argv...)
			}
		}
		ok := false
		for try := 1; try <= max(retries, 1); try++ {
			code, err := z.c.command(ctx, argv, out, nil)
			if err == nil && code == 0 {
				ok = true
				break
			}
			z.log.printf("%s: %s unreachable (try %d of %d)", name, h.addr, try, retries)
			if try < retries && sleep(ctx, interval) != nil {
				return 1, ctx.Err()
			}
		}
		if !ok {
			return 1, fmt.Errorf("connectivity-check: %s unreachable", h.addr)
		}
		z.log.printf("%s: %s reachable", name, h.addr)
	}
	return 0, nil
}

// snmp is the snmp plugin (E.6): community-ro (the twin also reads communities-ro, a string or
// a list) and snmp-location (also snmp-syslocation), at least one required, written to
// /etc/sonic/snmp.yml as snmp_rocommunity: and snmp_location: lines (other lines kept). When
// --cfgdb-set is given they are also written to CONFIG_DB, as `<cmd> SNMP_COMMUNITY|<c> TYPE RO`
// and `<cmd> SNMP|LOCATION Location <loc>`; without it the snmp.yml file is the record, logged.
// restart-agent is logged and not acted on: the snmp container is the image's.
func (z *sonic) snmp(ctx context.Context, name string, sec map[string]any) (int, error) {
	comms := strList(sec["community-ro"])
	if len(comms) == 0 {
		comms = strList(sec["communities-ro"])
	}
	loc := strOf(sec, "snmp-location")
	if loc == "" {
		loc = strOf(sec, "snmp-syslocation")
	}
	if len(comms) == 0 && loc == "" {
		return 1, errors.New("snmp: community-ro or snmp-location is required")
	}
	p := filepath.Join(z.c.sonicDir, "snmp.yml")
	var lines []string
	if b, err := os.ReadFile(p); err == nil {
		for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if len(comms) > 0 && strings.HasPrefix(l, "snmp_rocommunity:") || loc != "" && strings.HasPrefix(l, "snmp_location:") || l == "" {
				continue
			}
			lines = append(lines, l)
		}
	}
	if len(comms) > 0 {
		lines = append(lines, "snmp_rocommunity: "+comms[0])
	}
	if loc != "" {
		lines = append(lines, "snmp_location: "+loc)
	}
	if err := writeFileAtomic(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return 1, err
	}
	z.log.printf("%s: wrote %s", name, p)
	if z.c.cfgdbSet != "" {
		out := z.log.writer()
		defer out.Close()
		var sets [][]string
		for _, c := range comms {
			sets = append(sets, []string{"SNMP_COMMUNITY|" + c, "TYPE", "RO"})
		}
		if loc != "" {
			sets = append(sets, []string{"SNMP|LOCATION", "Location", loc})
		}
		for _, s := range sets {
			if code, err := z.c.shell(ctx, z.c.cfgdbSet, out, s...); err != nil || code != 0 {
				return max(code, 1), fmt.Errorf("snmp: %s %v: exit %d %v", z.c.cfgdbSet, s, code, err)
			}
		}
	} else {
		z.log.printf("%s: no --cfgdb-set: CONFIG_DB not written", name)
	}
	if boolOf(sec, "restart-agent", false) {
		z.log.printf("%s: restart-agent: the snmp agent is not restarted by the twin's ZTP", name)
	}
	return 0, nil
}

// downloadFiles is the download plugin (E.6): each of "files" is a url object, written to its
// destination.
func (z *sonic) downloadFiles(ctx context.Context, sec map[string]any) (int, error) {
	files, _ := sec["files"].([]any)
	if len(files) == 0 {
		return 1, errors.New(`download: no "files"`)
	}
	for _, f := range files {
		var spec urlSpec
		var err error
		if m, ok := f.(map[string]any); ok && m["dynamic-url"] != nil {
			spec, err = z.dynamicURL(ctx, m["dynamic-url"])
		} else if ok && m["url"] != nil {
			spec, err = parseURL(m["url"])
		} else {
			spec, err = parseURL(f)
		}
		if err != nil {
			return 1, err
		}
		if spec.destination == "" {
			return 1, fmt.Errorf("download: %s has no destination", spec.source)
		}
		b, err := z.download(ctx, spec)
		if err == nil {
			err = writeFileAtomic(spec.destination, b, 0o644)
		}
		if err != nil {
			return 1, err
		}
	}
	return 0, nil
}
