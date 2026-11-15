package nos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
	b, err := d.call(ctx, "POST", "/images/create?"+q.Encode(), nil, nil)
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
