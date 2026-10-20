package vm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/model"
)

// A machine's TPM is manufactured once, as a TPM vendor provisions a part before it ships: the
// first time its state directory is empty, swtpm_setup creates its endorsement keys (RSA 2048 and
// ECC) and writes their EK certificates, and a platform certificate naming the machine, into the
// TPM's NV indices (0x01c00002, 0x01c00016, 0x01c08000), signed by the estate CA through
// swtpm_localca. An OS that reads the EK certificate (NICo's scout does,
// with tpm2_getekcertificate and tpm2_nvread, and refuses a TPM without one) finds what a
// real TPM holds, and a stack that verifies it against a CA can be given the estate CA. The
// manufacturing log is kept beside the state (tpm-manufacture.log).

// manufactureTPM provisions an empty TPM state directory with EK and platform certificates.
// It reports what it did; an error leaves the state as it was (a TPM without certificates).
func (m *machine) manufactureTPM(state string) (string, error) {
	if _, err := os.Stat(filepath.Join(state, "tpm2-00.permall")); err == nil {
		return "", nil // manufactured on an earlier start
	}
	setup, err := exec.LookPath("swtpm_setup")
	if err != nil {
		return "", fmt.Errorf("swtpm_setup is not installed")
	}
	localca, err := exec.LookPath("swtpm_localca")
	if err != nil {
		return "", fmt.Errorf("swtpm_localca is not installed")
	}
	caCert, caKey := filepath.Join(m.a.rt.StateDir, "estate-ca.pem"), filepath.Join(m.a.rt.StateDir, "estate-ca.key")
	for _, f := range []string{caCert, caKey} {
		if _, err := os.Stat(f); err != nil {
			return "", fmt.Errorf("the estate CA: %w", err)
		}
	}
	dir := filepath.Join(m.dir, "tpm-manufacture")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	manuf, product, version := "", "", ""
	m.a.rt.Engine.Read(func(mm *model.Model) {
		d := mm.DeviceBy[m.path]
		if id := d.Profile.Spec.Identity; id != nil {
			manuf, product = id.Manufacturer, id.Model
		}
		if fw := d.State.Firmware["uefi"]; fw != nil {
			version = fw.Version
		}
	})
	opt := func(s string) string { return strings.ReplaceAll(orNone(s), " ", "_") }
	files := map[string]string{
		"localca.conf": fmt.Sprintf("statedir = %s\nsigningkey = %s\nissuercert = %s\ncertserial = %s\n",
			dir, caKey, caCert, filepath.Join(dir, "certserial")),
		"localca.options": fmt.Sprintf("--platform-manufacturer %s\n--platform-model %s\n--platform-version %s\n",
			opt(manuf), opt(product), opt(version)),
		"setup.conf": fmt.Sprintf("create_certs_tool = %s\ncreate_certs_tool_config = %s\ncreate_certs_tool_options = %s\nactive_pcr_banks = sha256\n",
			localca, filepath.Join(dir, "localca.conf"), filepath.Join(dir, "localca.options")),
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			return "", err
		}
	}
	logf := filepath.Join(m.dir, "tpm-manufacture.log")
	out, err := exec.Command(setup, "--tpm2", "--tpmstate", state, "--create-ek-cert", "--create-platform-cert",
		"--config", filepath.Join(dir, "setup.conf"), "--log", logf).CombinedOutput()
	if err != nil {
		for _, f := range []string{"tpm2-00.permall", ".lock"} {
			_ = os.Remove(filepath.Join(state, f))
		}
		b, _ := os.ReadFile(logf)
		return "", fmt.Errorf("swtpm_setup: %v: %s %s", err, strings.TrimSpace(string(out)), lastLines(string(b), 3))
	}
	return "EK certificates (RSA 2048, ECC) and a platform certificate signed by the estate CA", nil
}

func lastLines(s string, n int) string {
	l := strings.Split(strings.TrimSpace(s), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, " | ")
}
