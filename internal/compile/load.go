package compile

import (
	"fmt"
	"os"
	"runtime"

	"github.com/BudEcosystem/dc-twin/internal/pack"
	"github.com/BudEcosystem/dc-twin/internal/schema"
	"github.com/BudEcosystem/dc-twin/packs"
	"github.com/BudEcosystem/dc-twin/stacks"
)

// LoadEstate reads an estate file strictly.
func LoadEstate(path string) (*schema.Estate, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var e schema.Estate
	if err := schema.Decode(data, &e); err != nil {
		return nil, data, fmt.Errorf("%s: %w", path, err)
	}
	return &e, data, nil
}

// Registry loads the in-tree packs and stacks.
func Registry() (*pack.Registry, error) { return pack.Load(packs.FS, stacks.FS) }

// HostArch is this machine's architecture in the spec's spelling.
func HostArch() string {
	if runtime.GOARCH == "arm64" {
		return "aarch64"
	}
	return "x86_64"
}
