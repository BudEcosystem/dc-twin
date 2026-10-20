package ipmi

// The IPMI oracle (docs/spec/16-testing.md — Oracles per adapter): the real ipmitool, speaking
// `-I lanplus` to the lab estate, across vendors and both cipher suites, with the engine's state
// and event log checked after every request that changes something.

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/model"
	"github.com/BudEcosystem/dc-twin/internal/twin"
)

func ipmitoolBinary(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("ipmitool")
	if err != nil {
		t.Skip("ipmitool is not on PATH: the IPMI oracle needs it (brew install ipmitool, apt install ipmitool)")
	}
	return bin
}

type ipmitool struct {
	t                 *testing.T
	bin, host, port   string
	user, pass, suite string
}

func ipmitoolFor(t *testing.T, bin string, tw *twin.Twin, a *Adapter, path, suite string) ipmitool {
	host, port, err := net.SplitHostPort(a.Endpoint(path))
	if err != nil {
		t.Fatal(err)
	}
	cr := credsOf(tw, path)
	return ipmitool{t: t, bin: bin, host: host, port: port, user: cr.user, pass: cr.pass, suite: suite}
}

// run executes one ipmitool command. The password goes through IPMI_PASSWORD (-E), not argv.
func (x ipmitool) run(args ...string) (string, error) {
	base := []string{"-N", "1", "-R", "3", "-I", "lanplus", "-H", x.host, "-p", x.port, "-U", x.user, "-E"}
	if x.suite != "" {
		base = append(base, "-C", x.suite)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, x.bin, append(base, args...)...)
	cmd.Env = append(os.Environ(), "IPMI_PASSWORD="+x.pass)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (x ipmitool) must(args ...string) string {
	x.t.Helper()
	out, err := x.run(args...)
	x.t.Logf("$ ipmitool -I lanplus -C %s %s\n%s", x.suite, strings.Join(args, " "), out)
	if err != nil {
		x.t.Fatalf("ipmitool %s: %v", strings.Join(args, " "), err)
	}
	return out
}

func (x ipmitool) expect(out string, wants ...string) {
	x.t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			x.t.Errorf("output lacks %q", w)
		}
	}
}

func TestIpmitool(t *testing.T) {
	bin := ipmitoolBinary(t)
	tw, a := lab(t)

	targets := []struct {
		name, path, inject, mfr, fw, product, selEvent string
	}{
		{"dell-xe9680", dell, dell + "/psu-1", "Manufacturer ID           : 0\n", "Firmware Revision         : 7.10", "PowerEdge XE9680", "Power Supply"},
		{"gb300-tray", gb300, gb300, "Manufacturer ID           : 5703", "Firmware Revision         : 25.09", "NVIDIA GB300", "System Event"},
		{"generic-2u", generic, generic + "/psu-0", "Manufacturer ID           : 0\n", "Firmware Revision         : 1.12", "Generic 2U Server", "Power Supply"},
	}
	for _, tg := range targets {
		t.Run(tg.name, func(t *testing.T) {
			var d *model.Device
			tw.Engine.Read(func(m *model.Model) { d = m.DeviceBy[tg.path] })
			fault := "generic/power/psu-failure"
			if tg.name == "gb300-tray" {
				fault = "generic/leak/tray-detector"
			}
			if _, err := tw.Engine.Inject(tg.inject, fault, nil, "", stackSrc); err != nil {
				t.Fatal(err)
			}
			var sensors []string
			for _, s := range d.Profile.Spec.Sensors {
				if s.Quantity == "temperature" || s.Quantity == "power" {
					sensors = append(sensors, s.Name)
				}
			}

			for _, suite := range []string{"3", "17"} {
				x := ipmitoolFor(t, bin, tw, a, tg.path, suite)
				x.expect(x.must("mc", "info"), tg.mfr, tg.fw, "IPMI Version              : 2.0", "SDR Repository Device", "FRU Inventory Device")
				x.expect(x.must("chassis", "status"), "System Power         : on")
				x.expect(x.must("sel", "info"), "Entries          : 1", "Version          : 1.5")
				x.expect(x.must("sel", "list"), tg.selEvent, "Asserted")
				sdr := x.must("sdr", "list")
				for _, s := range sensors {
					x.expect(sdr, s)
				}
				x.expect(sdr, "degrees C", "Watts", "| ok")
				x.expect(x.must("sensor"), sensors...)
				x.expect(x.must("fru", "print"), "Product Name          : "+tg.product, "Product Serial        : "+d.Identity.Serial)
			}

			x := ipmitoolFor(t, bin, tw, a, tg.path, "17")
			onWithin := time.Duration(behaviour(d, "onSeconds")*tw.Engine.Scale()*float64(time.Second)) + 3*time.Second

			x.expect(x.must("chassis", "power", "status"), "Chassis Power is on")
			x.expect(x.must("chassis", "power", "off"), "Chassis Power Control: Down/Off")
			if p := view(tw, tg.path)["power"]; p != model.Off {
				t.Fatalf("after IPMI power off the model is %v", p)
			}
			evs := requests(tw, tg.path, "power.off.force")
			if len(evs) != 1 || evs[0].Kind != "request" || evs[0].Source.Principal != x.user {
				t.Fatalf("power.off.force request events: %+v", evs)
			}
			x.expect(x.must("chassis", "power", "status"), "Chassis Power is off")

			x.expect(x.must("chassis", "power", "on"), "Chassis Power Control: Up/On")
			if p := view(tw, tg.path)["power"]; p != model.PoweringOn {
				t.Fatalf("right after IPMI power on the model is %v", p)
			}
			waitFor(t, onWithin, "On after the scaled onSeconds", func() bool { return view(tw, tg.path)["power"] == model.On })
			x.expect(x.must("chassis", "power", "status"), "Chassis Power is on")

			x.expect(x.must("chassis", "power", "cycle"), "Chassis Power Control: Cycle")
			if len(requests(tw, tg.path, "power.cycle")) != 1 {
				t.Fatal("no power.cycle request event")
			}
			waitFor(t, onWithin, "On after the cycle", func() bool { return view(tw, tg.path)["power"] == model.On })

			x.expect(x.must("chassis", "bootdev", "pxe"), "Set Boot Device to pxe")
			if b := view(tw, tg.path)["boot"].(map[string]any); b["overrideTarget"] != "Pxe" || b["overrideEnabled"] != "Once" {
				t.Fatalf("boot after IPMI bootdev pxe: %v", b)
			}
			x.expect(x.must("chassis", "bootparam", "get", "5"), "Boot Flag Valid", "Options apply to only next boot", "Force PXE")
			// what Ironic's ipmitool driver sends before each boot device change
			x.must("raw", "0x00", "0x08", "0x03", "0x08")
			x.expect(x.must("chassis", "bootparam", "get", "3"), "Boot parameter data: 08", "Don't automatically clear boot flag valid bit on timeout")

			x.expect(x.must("sel", "clear"), "Clearing SEL")
			x.expect(x.must("sel", "list"), "SEL has no entries")

			bad := x
			bad.pass = "not-the-password"
			out, err := bad.run("chassis", "power", "status")
			t.Logf("$ ipmitool -I lanplus -C 17 -P <wrong> chassis power status\n%s(err: %v)", out, err)
			if err == nil || !strings.Contains(out, "Unable to establish IPMI v2 / RMCP+ session") {
				t.Fatalf("a wrong password was not refused")
			}
		})
	}

	t.Run("cipher-suite-discovery", func(t *testing.T) {
		// without -C, ipmitool asks Get Channel Cipher Suites and picks the strongest
		x := ipmitoolFor(t, bin, tw, a, generic, "")
		x.expect(x.must("channel", "getciphers", "ipmi", "1"), "hmac_sha1_96", "sha256_128", "aes_cbc_128")
		x.expect(x.must("chassis", "power", "status"), "Chassis Power is")
		if evs := requests(tw, generic, "session.create"); len(evs) == 0 || !strings.Contains(evs[len(evs)-1].Request.Target, "cipher suite 17") {
			t.Fatalf("auto-negotiated session: %+v", evs)
		}
	})
}

func behaviour(d *model.Device, key string) float64 {
	if v, ok := d.Profile.Spec.Behaviours["power"][key].(float64); ok {
		return v
	}
	panic(fmt.Sprintf("%s has no power.%s", d.ProfileName, key))
}
