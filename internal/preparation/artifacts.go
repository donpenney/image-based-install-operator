// Copyright 2026 Red Hat, Inc.
// SPDX-License-Identifier: Apache-2.0

package preparation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

const SecretISOKey = "config.iso"
const PayloadKey = "payload.json"

// Payload is private, immutable per-attempt input carried by cluster-config.
// Only Token's SHA-256 hash is persisted in the ICI status.
type Payload struct {
	Version     int       `json:"version"`
	AttemptID   string    `json:"attemptID"`
	HostUID     string    `json:"hostUID"`
	CallbackURL string    `json:"callbackURL"`
	Token       string    `json:"token"`
	CABundle    string    `json:"caBundle,omitempty"`
	Deadline    time.Time `json:"deadline"`
	RawNMState  string    `json:"rawNMState,omitempty"`
}

func SecretName(uid string, attempt int) string {
	return fmt.Sprintf("ibi-preparation-%s-%d", uid, attempt)
}
func BootPath(root, namespace, uid, attempt string) string {
	return filepath.Join(root, "preparation", namespace, uid, attempt, "boot.iso")
}

// SafeSegment validates Kubernetes-derived artifact path components before
// using persisted attempt state for file publication or recursive cleanup.
func SafeSegment(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\")
}

func BuildISO(payload Payload) ([]byte, error) {
	contents, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if len(contents) > 256*1024 {
		return nil, fmt.Errorf("preparation payload exceeds 256 KiB")
	}
	dir, err := os.MkdirTemp("", "ibi-preparation-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	tree := filepath.Join(dir, "contents")
	for _, name := range []string{"ibi-preparation", "cluster-configuration"} {
		if err = os.MkdirAll(filepath.Join(tree, name), 0700); err != nil {
			return nil, err
		}
	}
	if err = os.WriteFile(filepath.Join(tree, "ibi-preparation", "config.json"), contents, 0600); err != nil {
		return nil, err
	}
	manifest, err := json.Marshal(map[string]any{"api_version": 1, "raw_nm_state_config": payload.RawNMState})
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(tree, "cluster-configuration", "manifest.json"), manifest, 0600); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "config.iso")
	image, err := diskfs.Create(path, 38*1024, diskfs.Raw, diskfs.SectorSizeDefault)
	if err != nil {
		return nil, err
	}
	defer image.File.Close()
	image.LogicalBlocksize = 2048
	fs, err := image.CreateFilesystem(disk.FilesystemSpec{Partition: 0, FSType: filesystem.TypeISO9660, VolumeLabel: "cluster-config", WorkDir: tree})
	if err != nil {
		return nil, err
	}
	iso, ok := fs.(*iso9660.FileSystem)
	if !ok {
		return nil, fmt.Errorf("configuration image is not ISO9660")
	}
	if err = iso.Finalize(iso9660.FinalizeOptions{RockRidge: true, VolumeIdentifier: "cluster-config"}); err != nil {
		return nil, err
	}
	contents, err = os.ReadFile(path)
	if err == nil && len(contents) > 768*1024 {
		return nil, fmt.Errorf("preparation image exceeds the immutable Secret size budget")
	}
	return contents, err
}
