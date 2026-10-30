// Command catalogexport writes the exact embedded Barn image catalog for
// repository staging. Signing remains a separate catalogsign operation.
package main

import (
	"fmt"
	"os"

	"github.com/pgsty/barn/internal/fsutil"
	"github.com/pgsty/barn/internal/image"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: catalogexport <new-output-path>")
		os.Exit(2)
	}
	if err := exportCatalog(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func exportCatalog(target string) error {
	data, err := image.EmbeddedCatalogBytes()
	if err != nil {
		return err
	}
	target, err = fsutil.EnsureParent(target)
	if err != nil {
		return err
	}
	return fsutil.AtomicCreate(target, data, 0o600)
}
