package main

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pgsty/farrow/internal/activity"
	"github.com/pgsty/farrow/internal/image"
	"github.com/pgsty/farrow/internal/spec"
	"github.com/pgsty/farrow/internal/state"
)

// imageService resolves the deployment data root and returns the image
// façade over it.
func imageService(repository string, mirror bool, progress activity.Reporter) (image.Service, error) {
	dataRoot, err := state.ResolveDataRoot()
	if err != nil {
		return image.Service{}, err
	}
	return image.Service{DataRoot: dataRoot, Repository: repository, Mirror: mirror, Progress: progress}, nil
}

// checkInventoryImages resolves each node's image against the active local
// catalog without touching the network, so validate reports an unknown image,
// channel, or version with the choices instead of leaving it to plan or up.
func checkInventoryImages(ctx context.Context, resolved spec.Resolved) error {
	service, err := imageService("", false, nil)
	if err != nil {
		return err
	}
	session, err := service.OpenCatalog()
	if err != nil {
		return err
	}
	arch := lifecycleImageArch(resolved)
	for _, node := range resolved.Nodes {
		reference := node.Image
		if reference == "" {
			reference = resolved.Image
		}
		if _, err := session.LookupArch(ctx, reference, arch); err != nil {
			return fmt.Errorf("host %s (%s): %w", node.Address, node.Name, err)
		}
	}
	return nil
}

func validImageDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && value == fmt.Sprintf("%x", decoded)
}

// nodeImageReferences collects every image digest a deployment node still
// references, so prune never deletes a base image in use.
func nodeImageReferences(dataRoot string) (map[string]struct{}, error) {
	references := make(map[string]struct{})
	store := state.Store{Root: dataRoot}
	entries, readErr := os.ReadDir(filepath.Join(dataRoot, "nodes"))
	if errors.Is(readErr, os.ErrNotExist) {
		return references, nil
	}
	if readErr != nil {
		return nil, readErr
	}
	for _, entry := range entries {
		if transaction, transactionErr := store.ReadTransaction(entry.Name()); transactionErr == nil {
			return nil, fmt.Errorf("node %s has pending transaction %s", entry.Name(), transaction.OperationID)
		} else if !errors.Is(transactionErr, os.ErrNotExist) {
			return nil, transactionErr
		}
		node, nodeErr := store.ReadNode(entry.Name())
		if nodeErr != nil {
			return nil, fmt.Errorf("read node %s before image prune: %w", entry.Name(), nodeErr)
		}
		if !validImageDigest(node.Image.Digest) {
			return nil, fmt.Errorf("node %s has an invalid image digest", entry.Name())
		}
		references[node.Image.Digest] = struct{}{}
	}
	return references, nil
}
