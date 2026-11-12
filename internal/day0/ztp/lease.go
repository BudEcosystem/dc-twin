package ztp

import (
	"os"
	"strings"
)

// lease is eth0's DHCP lease as dhclient hands it to its hooks (dhclient-script(8)): reason,
// new_ip_address, new_routers, new_domain_name_servers and one new_<name> per option it was
// told to request, named as the NOS's dhclient.conf declares the option. The twin's exit hook
// writes that environment to /run/dctwin/dhcp-eth0.env as shell assignments, one per line
// (`new_ip_address='192.0.2.10'`), since the client does no DHCP of its own (D73).
type lease map[string]string

// The ZTP options under the names each NOS's dhclient.conf gives them. Cumulus declares option
// 239 as cumulus-provision-url (A.2); SONiC's dhclient.conf.j2 declares 239 as
// provisioning-script-url, 67 as bootfile-name and 66 as tftp-server-name (E.1). The ztp_* names
// are the twin's own, for an exit hook that writes the options under their ZTP meaning; the
// BOOTP file field (new_filename) stands in for option 67 when the server sets only that.
var (
	opt239 = []string{"new_cumulus_provision_url", "new_provisioning_script_url", "new_ztp_provisioning_script_url"}
	opt67  = []string{"new_ztp_json_url", "new_bootfile_name", "new_filename"}
	opt66  = []string{"new_tftp_server_name"}
)

// readLease reads the lease file; a missing file is no lease.
func readLease(path string) lease {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return parseEnv(string(b))
}

// bound reports whether the lease holds: dhclient runs its hooks with reason EXPIRE, FAIL,
// RELEASE or STOP when a lease is lost, and PREINIT before one is had, with no new_ variables
// worth reading.
func (l lease) bound() bool {
	if len(l) == 0 {
		return false
	}
	switch l["reason"] {
	case "EXPIRE", "FAIL", "RELEASE", "STOP", "PREINIT", "EXPIRE6", "RELEASE6", "STOP6", "PREINIT6":
		return false
	}
	return true
}

// option is the first of names the lease carries non-empty, or "" when the lease does not hold.
func (l lease) option(names []string) string {
	if !l.bound() {
		return ""
	}
	for _, n := range names {
		if v := strings.TrimSpace(l[n]); v != "" {
			return v
		}
	}
	return ""
}

// parseEnv parses shell assignments, one per line: `key=value`, `export key=value`, the value
// single-quoted (a quote inside written as quote, backslash, quote, quote), double-quoted (with its backslash
// escapes) or bare, and ending at unquoted whitespace (so a trailing comment is dropped).
// Lines that are not assignments are skipped.
func parseEnv(data string) lease {
	out := lease{}
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		if !validName(key) {
			continue
		}
		out[key] = shellValue(line[i+1:])
	}
	return out
}

func validName(s string) bool {
	for i, r := range s {
		if r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return s != ""
}

// shellValue is one shell word, unquoted as sh would.
func shellValue(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch c := s[i]; c {
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				b.WriteString(s[i+1:])
				return b.String()
			}
			b.WriteString(s[i+1 : i+1+j])
			i += j + 2
		case '"':
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`", s[i+1]) >= 0 {
					i++
				}
				b.WriteByte(s[i])
				i++
			}
			i++
		case '\\':
			if i+1 < len(s) {
				b.WriteByte(s[i+1])
			}
			i += 2
		case ' ', '\t', ';':
			return b.String()
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}
