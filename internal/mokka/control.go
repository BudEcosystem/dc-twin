package mokka

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/BudEcosystem/dc-twin/internal/pack"
)

// ControlSource reads a running twin through its control API (docs/spec/09-runtime.md): the
// device view for health, state and active faults, the event stream for changes. Fault shim
// effects come from the pack registry, which is compiled into this binary as into dctwin.
type ControlSource struct {
	Addr, Token string
	Reg         *pack.Registry
	HTTP        *http.Client
}

// ReadControl reads the control API's address and token from a state directory's control.json.
func ReadControl(path string) (addr, token string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	var info struct{ Addr, Token string }
	if err := json.Unmarshal(b, &info); err != nil {
		return "", "", fmt.Errorf("%s: %w", path, err)
	}
	if info.Addr == "" || info.Token == "" {
		return "", "", fmt.Errorf("%s: no address or token", path)
	}
	return info.Addr, info.Token, nil
}

func (s ControlSource) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func (s ControlSource) get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.Addr+"/v1"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	resp, err := s.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// DeviceView is the part of the control API's device view this package reads.
type DeviceView struct {
	Path     string `json:"path"`
	Identity struct {
		UUID string `json:"uuid"`
	} `json:"identity"`
	Components []struct {
		Path, Kind, Model, Vendor, Health, State string
	} `json:"components"`
	Faults []struct {
		ID, Fault, Target string
	} `json:"faults"`
}

// Device reads one device's view.
func (s ControlSource) Device(ctx context.Context, path string) (*DeviceView, error) {
	var v DeviceView
	if err := s.get(ctx, "/devices/"+path, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Check refuses a host the running twin does not hold as compiled here: the device's UUID is
// identity/v1 material of the estate's seed and the device's path, so equal UUIDs mean the same
// estate, and every GPU path must exist with the same product.
func (s ControlSource) Check(ctx context.Context, h Host) error {
	v, err := s.Device(ctx, h.Path)
	if err != nil {
		return err
	}
	if v.Identity.UUID != h.UUID {
		return fmt.Errorf("%s: the running twin's system UUID is %s, the estate compiled here gives %s: not the same estate or seed", h.Path, v.Identity.UUID, h.UUID)
	}
	for _, g := range h.GPUs {
		found := false
		for _, c := range v.Components {
			if c.Path == g.Path {
				found = c.Model == g.Name
			}
		}
		if !found {
			return fmt.Errorf("%s: the running twin has no GPU %s of %q", h.Path, g.Path, g.Name)
		}
	}
	return nil
}

// GPUs reads the host's GPU components and the faults on them.
func (s ControlSource) GPUs(ctx context.Context, h Host) ([]GPUState, error) {
	v, err := s.Device(ctx, h.Path)
	if err != nil {
		return nil, err
	}
	var out []GPUState
	for _, g := range h.GPUs {
		gs := GPUState{Index: g.Index, Path: g.Path}
		for _, c := range v.Components {
			if c.Path == g.Path {
				gs.Health, gs.State = c.Health, c.State
			}
		}
		for _, f := range v.Faults {
			if f.Target != g.Path {
				continue
			}
			fl := Fault{ID: f.ID, Fault: f.Fault}
			if s.Reg != nil {
				if ref := s.Reg.Faults[f.Fault]; ref != nil && ref.Spec != nil {
					fl.Title = ref.Spec.Title
					fl.Shims = ShimsOf(ref.Spec.Effects)
				}
			}
			gs.Faults = append(gs.Faults, fl)
		}
		out = append(out, gs)
	}
	return out, nil
}

// Changes follows the control API's event stream (server-sent events, one committed event per
// `data:` line), reconnecting while ctx lives.
func (s ControlSource) Changes(ctx context.Context) <-chan string {
	out := make(chan string, 64)
	go func() {
		defer close(out)
		for ctx.Err() == nil {
			s.stream(ctx, out)
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}()
	return out
}

func (s ControlSource) stream(ctx context.Context, out chan<- string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.Addr+"/v1/events/stream?reads=false", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	resp, err := (&http.Client{}).Do(req) // no timeout: the stream is long-lived
	if err != nil {
		return
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct{ Path string }
		if json.Unmarshal([]byte(line[6:]), &ev) == nil && ev.Path != "" {
			select {
			case out <- ev.Path:
			default:
			}
		}
	}
}
