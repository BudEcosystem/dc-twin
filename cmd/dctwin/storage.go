package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// storageCmd is `dctwin storage …`: the estate's shared storage systems (P4).
func storageCmd(a *api, args []string, jsonOut bool) error {
	usage := fmt.Errorf("storage ls | get <system> | credentials | create filesystem|volume|bucket|snapshot … | delete <kind> <system> <name> | map|unmap <system> <volume> <host NQN> | fill <system> <object> <bytes|N%%> | offline|online <system> <object>")
	if len(args) == 0 {
		return usage
	}
	out := func(b []byte, err error) error {
		if err != nil {
			return err
		}
		os.Stdout.Write(b)
		return nil
	}
	post := func(body map[string]any) error { return out(a.do("POST", "/storage", body, "")) }
	size, args := flagVal(args, "--size")
	var exports []map[string]any
	for {
		var x string
		x, args = flagVal(args, "--export")
		if x == "" {
			break
		}
		parts := strings.Split(x, ":")
		e := map[string]any{"clients": parts[0], "access": "rw", "rootSquash": true}
		for _, p := range parts[1:] {
			switch p {
			case "ro", "rw":
				e["access"] = p
			case "root":
				e["rootSquash"] = false
			}
		}
		exports = append(exports, e)
	}
	switch args[0] {
	case "ls":
		b, err := a.do("GET", "/storage", nil, "")
		if err != nil || jsonOut {
			return out(b, err)
		}
		var vs []map[string]any
		json.Unmarshal(b, &vs)
		for _, v := range vs {
			addr, _ := v["Addresses"].(map[string]any)
			fmt.Printf("%-32s %-24s %-9s %-8s %5.1f%% of %-10s nodes %-3d fs %-3d vol %-3d buckets %-3d alerts %-3d mgmt %v data %v\n",
				v["Path"], v["Template"], v["State"], v["Health"], v["UsedPercent"], human(v["UsableBytes"]), count(v["Nodes"]),
				count(v["FileSystems"]), count(v["Volumes"]), count(v["Buckets"]), count(v["Alerts"]), addr["management"], addr["data"])
		}
		return nil
	case "get":
		if len(args) != 2 {
			return usage
		}
		return out(a.do("GET", "/storage?system="+url.QueryEscape(args[1]), nil, ""))
	case "credentials":
		return out(a.do("GET", "/storage/credentials", nil, ""))
	case "create":
		if len(args) < 4 {
			return usage
		}
		body := map[string]any{"op": "create", "kind": args[1], "system": args[2], "name": args[3]}
		if args[1] == "snapshot" {
			body["of"], body["name"] = args[3], ""
			if len(args) > 4 {
				body["name"] = args[4]
			}
		}
		if size != "" {
			n, err := parseSize(size)
			if err != nil {
				return err
			}
			body["size"] = n
		}
		if exports != nil {
			body["exports"] = exports
		}
		return post(body)
	case "delete":
		if len(args) != 4 {
			return usage
		}
		return post(map[string]any{"op": "delete", "kind": args[1], "system": args[2], "name": args[3]})
	case "map", "unmap":
		if len(args) != 4 {
			return usage
		}
		return post(map[string]any{"op": args[0], "system": args[1], "name": args[2], "host": args[3]})
	case "register", "unregister":
		if len(args) != 3 {
			return usage
		}
		return post(map[string]any{"op": args[0], "system": args[1], "host": args[2]})
	case "fill":
		if len(args) != 4 {
			return usage
		}
		body := map[string]any{"op": "fill", "system": args[1], "name": args[2]}
		if p, ok := strings.CutSuffix(args[3], "%"); ok {
			f, err := strconv.ParseFloat(p, 64)
			if err != nil {
				return err
			}
			body["usedPercent"] = f
		} else {
			n, err := parseSize(args[3])
			if err != nil {
				return err
			}
			body["usedBytes"] = n
		}
		return post(body)
	case "offline", "online":
		if len(args) != 3 {
			return usage
		}
		return post(map[string]any{"op": args[0], "system": args[1], "name": args[2]})
	}
	return usage
}

// parseSize reads 1099511627776, 10T, 500G or 1Ti (decimal units, or binary with i).
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	mult := int64(1)
	bin := strings.HasSuffix(s, "i")
	t := strings.TrimSuffix(strings.TrimSuffix(s, "i"), "B")
	if t != "" {
		units := "KMGTP"
		if i := strings.IndexByte(units, t[len(t)-1]); i >= 0 {
			base := int64(1000)
			if bin {
				base = 1024
			}
			for k := 0; k <= i; k++ {
				mult *= base
			}
			t = t[:len(t)-1]
		}
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: a number of bytes, or with K, M, G, T or P (Ki … Pi binary)", s)
	}
	return int64(f * float64(mult)), nil
}

func human(v any) string {
	f, _ := v.(float64)
	switch {
	case f >= 1e15:
		return fmt.Sprintf("%.2fPB", f/1e15)
	case f >= 1e12:
		return fmt.Sprintf("%.2fTB", f/1e12)
	}
	return fmt.Sprintf("%.0fGB", f/1e9)
}

func count(v any) int {
	l, _ := v.([]any)
	return len(l)
}

// dpuCmd is `dctwin dpu …`: a DPU's NVMe emulation (P5).
func dpuCmd(a *api, args []string) error {
	usage := fmt.Errorf("dpu emulation <dpu> [on|off] | attach <dpu> <system> <volume> | detach <dpu> <function id>")
	out := func(b []byte, err error) error {
		if err != nil {
			return err
		}
		os.Stdout.Write(b)
		return nil
	}
	post := func(body map[string]any) error { return out(a.do("POST", "/dpu/emulation", body, "")) }
	switch {
	case len(args) == 2 && args[0] == "emulation":
		return out(a.do("GET", "/dpu/emulation?device="+url.QueryEscape(args[1]), nil, ""))
	case len(args) == 3 && args[0] == "emulation":
		return post(map[string]any{"op": args[2], "device": args[1]})
	case len(args) == 4 && args[0] == "attach":
		return post(map[string]any{"op": "attach", "device": args[1], "system": args[2], "volume": args[3]})
	case len(args) == 3 && args[0] == "detach":
		id, err := strconv.Atoi(args[2])
		if err != nil {
			return usage
		}
		return post(map[string]any{"op": "detach", "device": args[1], "id": id})
	}
	return usage
}
