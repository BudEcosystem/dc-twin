package nos

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// The twin's own images (its NOSes' and their sidecars': dctwin-nos-nvue-frr:bookworm,
// dctwin-sonic-gnmi:…) are named with no registry, as scripts/linux-substrate-e2e.sh builds them.
// A Docker host that has not built them can pull them from a registry they are published to
// (spec.adapters.network.nos.registry, else DCTWIN_REGISTRY): <registry>/<name>:<tag>, then
// tagged as the twin names them, so everything that runs them is unchanged. An image named with
// a registry (SONiC's switch image, by digest) is pulled from its own.

// ErrNotPresent is an image the Docker host does not have and the twin could not pull: one of
// its own with no registry set, or one whose pull failed.
var ErrNotPresent = errors.New("not present on the Docker host")

// Own reports whether an image is one of the twin's own: named with no registry, or with
// Podman's localhost/ for a locally built one.
func Own(image string) bool {
	first, _, nested := strings.Cut(image, "/")
	return !nested || first == "localhost"
}

// FromRegistry is where one of the twin's own images is pulled from: <registry>/<name>, the
// name without Podman's localhost/.
func FromRegistry(registry, image string) string {
	return strings.TrimRight(registry, "/") + "/" + strings.TrimPrefix(image, "localhost/")
}

// ValidRegistry checks a registry as the twin takes one: a host with an optional port and path
// (registry.bud.studio/gpu-foundry, registry.lab:5000/dc-twin), no scheme, no tag.
func ValidRegistry(registry string) error {
	switch {
	case registry == "":
		return nil
	case strings.Contains(registry, "://"):
		return fmt.Errorf("%q: a registry is named without a scheme (registry.bud.studio/gpu-foundry)", registry)
	case strings.ContainsAny(registry, " @"):
		return fmt.Errorf("%q: not a registry", registry)
	}
	host, _, _ := strings.Cut(strings.TrimRight(registry, "/"), "/")
	if !strings.ContainsAny(host, ".:") && host != "localhost" {
		return fmt.Errorf("%q: a registry's first part is its host (registry.bud.studio, registry.lab:5000, localhost:5000)", registry)
	}
	return nil
}

// Ensure makes an image present on the Docker host (for platform, when one is given): there
// already; one of the twin's own, with a registry given, pulled from <registry>/<image> and
// tagged as image; one named with a registry, pulled from it (pinned by digest, as SONiC's switch
// image is, it is the same image wherever it is pulled). It returns what it pulled ("" when
// nothing), and ErrNotPresent, wrapped, when the image is missing and nothing could be pulled.
func (d *Docker) Ensure(ctx context.Context, image, registry, platform string) (string, error) {
	if d.HasImage(ctx, image) {
		return "", nil
	}
	from := image
	if Own(image) {
		if registry == "" {
			return "", ErrNotPresent
		}
		from = FromRegistry(registry, image)
	}
	if err := d.Pull(ctx, from, platform); err != nil {
		return "", fmt.Errorf("%w, and pulling it from %s failed: %w", ErrNotPresent, from, err)
	}
	if from != image {
		if err := d.Tag(ctx, from, strings.TrimPrefix(image, "localhost/")); err != nil {
			return "", fmt.Errorf("pulled %s, but tagging it %s failed: %w", from, image, err)
		}
	}
	return from, nil
}

// Pull pulls an image (repo:tag, or repo@sha256:…) from its registry, for platform when one is
// given (linux/amd64). The Engine answers a pull with 200 and a progress stream, one JSON object
// a line, and reports a failure in that stream: it is read to its end.
func (d *Docker) Pull(ctx context.Context, ref, platform string) error {
	repo, tag := splitRef(ref)
	q := url.Values{"fromImage": {repo}, "tag": {tag}}
	if platform != "" {
		q.Set("platform", platform)
	}
	var hdr http.Header
	if a := registryAuth(repo); a != "" {
		hdr = http.Header{"X-Registry-Auth": {a}}
	}
	b, err := d.callWith(ctx, "POST", "/images/create?"+q.Encode(), hdr, nil, nil)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	for {
		var m struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := dec.Decode(&m); err != nil {
			return nil // the stream's end
		}
		if why := m.ErrorDetail.Message; why != "" {
			return errors.New(why)
		}
		if m.Error != "" {
			return errors.New(m.Error)
		}
	}
}

// Tag names an image as another: repo:tag.
func (d *Docker) Tag(ctx context.Context, image, as string) error {
	repo, tag := splitRef(as)
	q := url.Values{"repo": {repo}, "tag": {tag}}
	_, err := d.call(ctx, "POST", "/images/"+url.PathEscape(image)+"/tag?"+q.Encode(), nil, nil)
	return err
}

// splitRef splits an image reference into its repository and its tag or digest: a digest
// follows an @; a tag, the last colon after the last slash (a registry's port comes before it).
// A reference with neither is latest.
func splitRef(ref string) (repo, tag string) {
	if r, digest, ok := strings.Cut(ref, "@"); ok {
		return r, digest
	}
	slash := strings.LastIndex(ref, "/")
	if i := strings.LastIndex(ref, ":"); i > slash {
		return ref[:i], ref[i+1:]
	}
	return ref, "latest"
}

// registryAuth is the X-Registry-Auth header for a pull of repo, as the Docker CLI sends it: the
// Engine API logs in to a registry only with what its client sends (Podman's too, though its own
// CLI reads its auth file). The credentials are the login the auth file holds for repo's registry:
// REGISTRY_AUTH_FILE (Podman's, which a kubernetes.io/dockerconfigjson Secret is the form of), else
// $DOCKER_CONFIG/config.json, else ~/.docker/config.json. "" when there is none; credential helpers
// are not read.
func registryAuth(repo string) string {
	path := os.Getenv("REGISTRY_AUTH_FILE")
	if path == "" {
		dir := os.Getenv("DOCKER_CONFIG")
		if dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return ""
			}
			dir = filepath.Join(home, ".docker")
		}
		path = filepath.Join(dir, "config.json")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var f struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if json.Unmarshal(b, &f) != nil {
		return ""
	}
	host, _, nested := strings.Cut(repo, "/")
	if !nested || !strings.ContainsAny(host, ".:") && host != "localhost" {
		host = "docker.io"
	}
	keys := []string{host, "https://" + host, "http://" + host}
	server := host
	if host == "docker.io" {
		keys = append(keys, "https://index.docker.io/v1/", "index.docker.io")
		server = "https://index.docker.io/v1/" // Docker Hub's, as the Docker CLI names it
	}
	for _, k := range keys {
		a, ok := f.Auths[k]
		if !ok {
			continue
		}
		user, pass := a.Username, a.Password
		if a.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(a.Auth)
			if err != nil {
				continue // unreadable: another spelling of the registry may hold a usable login
			}
			user, pass, _ = strings.Cut(string(raw), ":")
		}
		j, _ := json.Marshal(map[string]string{"username": user, "password": pass, "serveraddress": server})
		return base64.URLEncoding.EncodeToString(j)
	}
	return ""
}
