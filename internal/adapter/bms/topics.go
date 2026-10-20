package bms

import "strings"

// Match reports whether an MQTT topic matches a filter (MQTT 3.1.1 §4.7): "+" matches exactly
// one level, a trailing "#" matches the parent level and every level below it, and a wildcard
// in the first level never matches a topic beginning with "$".
func Match(filter, topic string) bool {
	if filter == "" || topic == "" {
		return false
	}
	f := strings.Split(filter, "/")
	t := strings.Split(topic, "/")
	if strings.HasPrefix(topic, "$") && (f[0] == "+" || f[0] == "#") {
		return false
	}
	for i, level := range f {
		if level == "#" {
			return i == len(f)-1
		}
		if i >= len(t) {
			return false
		}
		if level != "+" && level != t[i] {
			return false
		}
	}
	return len(f) == len(t)
}

// expand fills a dialect topic template.
func expand(tmpl string, vars map[string]string) string {
	out := tmpl
	for k, v := range vars {
		out = strings.ReplaceAll(out, "{"+k+"}", v)
	}
	return out
}

// integrationTopic derives where an integration publishes a point's value from the BMS's
// metadata topic, by the AsyncAPI's rule: PUB → {integration}, Metadata → Value, the rest kept.
func integrationTopic(metadataTopic, integration string) (string, bool) {
	const pub = "/PUB/Metadata/"
	i := strings.Index(metadataTopic, pub)
	if i < 0 {
		return "", false
	}
	return metadataTopic[:i] + "/" + integration + "/Value/" + metadataTopic[i+len(pub):], true
}

// tagSafe makes one tagPath level safe for MQTT and for a NATS subject: no wildcards, no
// empty levels, and no '.' (NATS maps it to "//") or spaces.
func tagSafe(s string) string {
	r := strings.NewReplacer("+", "_", "#", "_", ".", "_", " ", "_", "$", "_")
	return r.Replace(s)
}

// tagPath joins path levels into a tagPath, dropping empty levels.
func tagPath(parts ...string) string {
	var out []string
	for _, p := range parts {
		for _, l := range strings.Split(p, "/") {
			if l != "" {
				out = append(out, tagSafe(l))
			}
		}
	}
	return strings.Join(out, "/")
}
