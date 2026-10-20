package nos

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Docker is a NOS runtime on a Docker Engine, spoken to over its unix socket (the Engine API,
// no CLI): the twin creates one container per real switch with no network, and moves the
// switch's ports into the container's network namespace itself, as containerlab does. Opening
// that namespace (/proc/<pid>/ns/net) needs the twin to see the container's process: it runs on
// the Docker host, or in a container sharing its PID namespace. Podman's Docker-compatible API
// (`podman system service`, rootful) serves as well; Podman is recorded where the two differ.
type Docker struct {
	Socket string
	// Podman: the socket is Podman's Docker-compatible API.
	Podman bool
	c      *http.Client
}

// Container is a started switch container.
type Container struct {
	ID, Name string
	Pid      int
}

// NewDocker returns a runtime on DOCKER_HOST (unix only) or /var/run/docker.sock, and checks it
// answers.
func NewDocker() (*Docker, error) {
	sock := "/var/run/docker.sock"
	if h := os.Getenv("DOCKER_HOST"); h != "" {
		if !strings.HasPrefix(h, "unix://") {
			return nil, fmt.Errorf("DOCKER_HOST %s: only a unix socket is supported", h)
		}
		sock = strings.TrimPrefix(h, "unix://")
	}
	if _, err := os.Stat(sock); err != nil {
		return nil, fmt.Errorf("no Docker Engine socket at %s: %w", sock, err)
	}
	d := &Docker{Socket: sock, c: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dd net.Dialer
			return dd.DialContext(ctx, "unix", sock)
		}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := d.call(ctx, "GET", "/_ping", nil, nil); err != nil {
		return nil, fmt.Errorf("the Docker Engine at %s does not answer: %w", sock, err)
	}
	var v struct{ Components []struct{ Name string } }
	if _, err := d.call(ctx, "GET", "/version", nil, &v); err == nil {
		for _, c := range v.Components {
			d.Podman = d.Podman || strings.Contains(c.Name, "Podman")
		}
	}
	return d, nil
}

func (d *Docker) call(ctx context.Context, method, path string, body, out any) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		var m struct{ Message string }
		_ = json.Unmarshal(b, &m)
		if m.Message == "" {
			m.Message = strings.TrimSpace(string(b))
		}
		return b, &APIError{Status: resp.StatusCode, Message: m.Message}
	}
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return b, err
		}
	}
	return b, nil
}

// APIError is an Engine API refusal.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("docker: %d %s", e.Status, e.Message) }

// HasImage reports whether an image is present locally.
func (d *Docker) HasImage(ctx context.Context, image string) bool {
	_, err := d.call(ctx, "GET", "/images/"+url.PathEscape(image)+"/json", nil, nil)
	return err == nil
}

// Spec is what a switch container (or its relay container) is created with.
type Spec struct {
	Name, Image, Hostname string
	Entrypoint, Cmd       []string
	Env                   []string
	Labels                map[string]string
	Sysctls               map[string]string
	// Platform asks for the image's platform (linux/amd64), which an emulated image needs.
	Platform string
	// Privileged runs it privileged (SONiC-VS); otherwise it gets the capabilities a switch needs.
	Privileged bool
	// NetworkMode is "none" (the default: the twin wires it) or "container:<id>", sharing a
	// switch's network namespace and its hostname.
	NetworkMode string
	// Binds mounts volumes ("<volume>:<path>[:ro]").
	Binds []string
	// Tmpfs mounts tmpfs file systems, by path.
	Tmpfs map[string]string
	// CapAdd replaces the capabilities a non-privileged switch container gets.
	CapAdd []string
}

// switchCaps are what a switch container needs when it is not privileged: it configures its own
// interfaces, bridges and routes, and its relay reads raw frames.
var switchCaps = []string{"NET_ADMIN", "NET_RAW", "NET_BIND_SERVICE", "SYS_ADMIN"}

// Run creates and starts a container (with no network unless it joins a switch's), removing any
// leftover of the same name first.
func (d *Docker) Run(ctx context.Context, s Spec) (*Container, error) {
	_ = d.Remove(ctx, s.Name)
	hc := map[string]any{"NetworkMode": "none", "Sysctls": s.Sysctls, "Binds": s.Binds, "Tmpfs": s.Tmpfs}
	if s.NetworkMode != "" {
		hc["NetworkMode"] = s.NetworkMode
		if strings.HasPrefix(s.NetworkMode, "container:") && d.Podman {
			// Docker gives a container in another's network namespace that container's hostname;
			// Podman does only when it joins its UTS namespace too, and a relay's circuit id
			// carries the hostname (SONiC's %h)
			hc["UTSMode"] = s.NetworkMode
		}
	}
	if s.Privileged {
		hc["Privileged"] = true
	} else if s.CapAdd != nil {
		hc["CapAdd"] = s.CapAdd
	} else {
		hc["CapAdd"] = switchCaps
	}
	body := map[string]any{"Image": s.Image, "Cmd": s.Cmd, "Env": s.Env, "Labels": s.Labels, "HostConfig": hc}
	if s.Entrypoint != nil {
		body["Entrypoint"] = s.Entrypoint
	}
	if !strings.HasPrefix(s.NetworkMode, "container:") {
		body["Hostname"] = s.Hostname // a container in another's namespace has its hostname
	}
	q := "name=" + url.QueryEscape(s.Name)
	if s.Platform != "" {
		q += "&platform=" + url.QueryEscape(s.Platform)
	}
	var created struct{ Id string }
	if _, err := d.call(ctx, "POST", "/containers/create?"+q, body, &created); err != nil {
		return nil, fmt.Errorf("create %s from %s: %w", s.Name, s.Image, err)
	}
	if _, err := d.call(ctx, "POST", "/containers/"+created.Id+"/start", nil, nil); err != nil {
		_ = d.Remove(ctx, created.Id)
		return nil, fmt.Errorf("start %s: %w", s.Name, err)
	}
	c := &Container{ID: created.Id, Name: s.Name}
	pid, err := d.Pid(ctx, c.ID)
	if err != nil {
		_ = d.Remove(ctx, created.Id)
		return nil, err
	}
	c.Pid = pid
	return c, nil
}

// CanRun checks that the Docker host runs an image of a foreign platform: it runs /bin/true from
// the image, in a container of the name given, and wants it to exit 0. An aarch64 host without
// binfmt_misc for amd64 fails here, with exec format error, rather than in a switch that never
// boots.
func (d *Docker) CanRun(ctx context.Context, name, image, platform string) error {
	_ = d.Remove(ctx, name)
	q := "name=" + url.QueryEscape(name)
	if platform != "" {
		q += "&platform=" + url.QueryEscape(platform)
	}
	body := map[string]any{"Image": image, "Entrypoint": []string{"/bin/true"}, "Cmd": []string{}, "HostConfig": map[string]any{"NetworkMode": "none"}}
	var created struct{ Id string }
	if _, err := d.call(ctx, "POST", "/containers/create?"+q, body, &created); err != nil {
		return err
	}
	defer d.Remove(context.Background(), created.Id)
	if _, err := d.call(ctx, "POST", "/containers/"+created.Id+"/start", nil, nil); err != nil {
		return err
	}
	return d.exited(ctx, created.Id)
}

// exited waits for a container to stop and returns an error unless it exited 0, with its output.
func (d *Docker) exited(ctx context.Context, id string) error {
	var res struct{ StatusCode int }
	if _, err := d.call(ctx, "POST", "/containers/"+url.PathEscape(id)+"/wait?condition=not-running", nil, &res); err != nil {
		return err
	}
	if res.StatusCode != 0 {
		logs, _ := d.Logs(ctx, id)
		return fmt.Errorf("exit %d: %s", res.StatusCode, strings.TrimSpace(logs))
	}
	return nil
}

// CreateVolume creates a named volume, labelled (so a crashed run's are found by label); one left
// over by that name is removed first.
func (d *Docker) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	_ = d.RemoveVolume(ctx, name)
	_, err := d.call(ctx, "POST", "/volumes/create", map[string]any{"Name": name, "Labels": labels}, nil)
	return err
}

// RemoveVolume removes a named volume; a missing one is not an error.
func (d *Docker) RemoveVolume(ctx context.Context, name string) error {
	_, err := d.call(ctx, "DELETE", "/volumes/"+url.PathEscape(name)+"?force=1", nil, nil)
	if ae, ok := err.(*APIError); ok && ae.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// Pid is a running container's init process id.
func (d *Docker) Pid(ctx context.Context, id string) (int, error) {
	var info struct {
		State struct {
			Pid     int
			Running bool
		}
	}
	if _, err := d.call(ctx, "GET", "/containers/"+url.PathEscape(id)+"/json", nil, &info); err != nil {
		return 0, err
	}
	if !info.State.Running || info.State.Pid == 0 {
		return 0, fmt.Errorf("container %s is not running", id)
	}
	return info.State.Pid, nil
}

// Exec runs a command in a container and returns its combined output and exit code.
func (d *Docker) Exec(ctx context.Context, id string, cmd ...string) (string, int, error) {
	var ex struct{ Id string }
	if _, err := d.call(ctx, "POST", "/containers/"+url.PathEscape(id)+"/exec",
		map[string]any{"Cmd": cmd, "AttachStdout": true, "AttachStderr": true, "Tty": true}, &ex); err != nil {
		return "", -1, err
	}
	out, err := d.call(ctx, "POST", "/exec/"+ex.Id+"/start", map[string]any{"Detach": false, "Tty": true}, nil)
	if err != nil {
		return string(out), -1, err
	}
	var st struct{ ExitCode int }
	if _, err := d.call(ctx, "GET", "/exec/"+ex.Id+"/json", nil, &st); err != nil {
		return string(out), -1, err
	}
	return strings.ReplaceAll(string(out), "\r\n", "\n"), st.ExitCode, nil
}

// ExecDetached starts a command in a container and returns at once; it runs until it exits (a
// packet capture beside a switch's daemons, say).
func (d *Docker) ExecDetached(ctx context.Context, id string, cmd ...string) error {
	var ex struct{ Id string }
	if _, err := d.call(ctx, "POST", "/containers/"+url.PathEscape(id)+"/exec", map[string]any{"Cmd": cmd}, &ex); err != nil {
		return err
	}
	_, err := d.call(ctx, "POST", "/exec/"+ex.Id+"/start", map[string]any{"Detach": true}, nil)
	return err
}

// Logs is a container's output so far (stdout and stderr, demultiplexed).
func (d *Docker) Logs(ctx context.Context, id string) (string, error) {
	b, err := d.call(ctx, "GET", "/containers/"+url.PathEscape(id)+"/logs?stdout=1&stderr=1", nil, nil)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	for len(b) >= 8 && (b[0] == 1 || b[0] == 2) {
		n := int(binary.BigEndian.Uint32(b[4:8]))
		if 8+n > len(b) {
			break
		}
		out.Write(b[8 : 8+n])
		b = b[8+n:]
	}
	out.Write(b)
	return out.String(), nil
}

// Remove force-removes a container (by id or name); a missing one is not an error.
func (d *Docker) Remove(ctx context.Context, id string) error {
	_, err := d.call(ctx, "DELETE", "/containers/"+url.PathEscape(id)+"?force=1&v=1", nil, nil)
	if ae, ok := err.(*APIError); ok && ae.Status == http.StatusNotFound {
		return nil
	}
	return err
}
