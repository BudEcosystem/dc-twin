package twin

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/pack"
)

// AssetsDir is where obtained assets live: $DCTWIN_ASSETS, else ~/.cache/dc-twin/assets.
func AssetsDir() string {
	if d := os.Getenv("DCTWIN_ASSETS"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "dc-twin", "assets")
}

// AssetPath resolves "<asset>/<sub>" against the packs' asset declarations.
func AssetPath(reg *pack.Registry, ref string) (string, error) {
	name, sub, _ := strings.Cut(ref, "/")
	for _, p := range reg.Packs {
		for _, a := range p.Manifest.Spec.Assets {
			if a.Name != name || a.Obtain == nil {
				continue
			}
			dir := filepath.Join(AssetsDir(), name+"@"+a.Obtain.Ref, sub)
			if _, err := os.Stat(dir); err != nil {
				return dir, fmt.Errorf("asset %s is not present at %s; run `dctwin assets fetch` (licence %s, from %s@%s)", ref, dir, a.Licence, a.Obtain.Source, a.Obtain.Ref)
			}
			return dir, nil
		}
	}
	return "", fmt.Errorf("asset %q is declared by no pack", name)
}

// FetchAssets downloads every declared asset that is not yet present.
func FetchAssets(reg *pack.Registry, w io.Writer) error {
	done := map[string]bool{}
	for _, p := range reg.Packs {
		for _, a := range p.Manifest.Spec.Assets {
			if a.Obtain == nil {
				continue
			}
			key := a.Name + "@" + a.Obtain.Ref
			if done[key] {
				continue
			}
			done[key] = true
			dst := filepath.Join(AssetsDir(), key)
			if _, err := os.Stat(dst); err == nil {
				fmt.Fprintf(w, "%s: present at %s\n", key, dst)
				continue
			}
			repo := strings.TrimPrefix(a.Obtain.Source, "https://github.com/")
			if len(a.Obtain.Files) > 0 {
				base := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s", repo, a.Obtain.Ref, a.Obtain.Path)
				fmt.Fprintf(w, "%s: fetching %s from %s (licence %s)\n", key, strings.Join(a.Obtain.Files, ", "), base, a.Licence)
				if err := fetchFiles(base, a.Obtain.Files, dst); err != nil {
					return fmt.Errorf("%s: %w", key, err)
				}
				continue
			}
			url := fmt.Sprintf("https://codeload.github.com/%s/tar.gz/%s", repo, a.Obtain.Ref)
			fmt.Fprintf(w, "%s: fetching %s (path %s, licence %s)\n", key, url, a.Obtain.Path, a.Licence)
			if err := fetchTarball(url, a.Obtain.Path, dst); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	return nil
}

func fetchTarball(url, sub, dst string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	tmp := dst + ".partial"
	os.RemoveAll(tmp)
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		// strip "<repo>-<ref>/" then keep only <sub>/
		_, rest, ok := strings.Cut(h.Name, "/")
		if !ok || !strings.HasPrefix(rest, sub+"/") {
			continue
		}
		rel := strings.TrimPrefix(rest, sub+"/")
		if rel == "" || strings.Contains(rel, "..") {
			continue
		}
		target := filepath.Join(tmp, rel)
		switch h.Typeflag {
		case tar.TypeDir:
			os.MkdirAll(target, 0o755)
		case tar.TypeReg:
			os.MkdirAll(filepath.Dir(target), 0o755)
			f, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return os.Rename(tmp, dst)
}

// fetchFiles obtains named files from base (a raw-file URL prefix at a pinned commit) into dst,
// all or none.
func fetchFiles(base string, files []string, dst string) error {
	tmp := dst + ".partial"
	os.RemoveAll(tmp)
	for _, f := range files {
		if f == "" || strings.Contains(f, "..") || strings.HasPrefix(f, "/") {
			return fmt.Errorf("asset file %q is not a relative path", f)
		}
		resp, err := http.Get(base + "/" + f)
		if err != nil {
			return err
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return fmt.Errorf("GET %s/%s: %s", base, f, resp.Status)
		}
		target := filepath.Join(tmp, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			resp.Body.Close()
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			resp.Body.Close()
			return err
		}
		_, err = io.Copy(out, resp.Body)
		resp.Body.Close()
		out.Close()
		if err != nil {
			return err
		}
	}
	return os.Rename(tmp, dst)
}
