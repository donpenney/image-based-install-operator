// Copyright 2026 Red Hat, Inc.
// SPDX-License-Identifier: Apache-2.0

// This development tool prepares and checks the simultaneous-media experiment.
// It performs no Kubernetes, BMC, or target disk operations.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/iso9660"
)

const preparationService = "install-rhcos-and-restore-seed.service"

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: preprovisioning {mask-ignition|config-iso|check-media} [flags]")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	switch args[0] {
	case "mask-ignition":
		input := flags.String("input", "", "complete Ignition extracted from the existing live ISO")
		output := flags.String("output", "", "new Ignition file; must not exist")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *input == "" || *output == "" || flags.NArg() != 0 {
			return errors.New("mask-ignition requires --input and --output")
		}
		content, err := os.ReadFile(*input) // #nosec G304 G703 -- Explicit local CLI input, never a remote request.
		if err != nil {
			return err
		}
		masked, err := maskPreparation(content)
		if err != nil {
			return err
		}
		return writeNew(*output, masked)
	case "config-iso":
		output := flags.String("output", "", "new diagnostic configuration ISO; must not exist")
		marker := flags.String("marker", "", "unique experiment marker")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *output == "" || flags.NArg() != 0 {
			return errors.New("config-iso requires --output and --marker")
		}
		return createProbeISO(*output, *marker)
	case "check-media":
		input := flags.String("input", "", "expanded Redfish VirtualMedia JSON collection or array")
		boot := flags.String("boot-url", "", "actual BMC-facing boot ISO URL")
		data := flags.String("data-url", "", "actual BMC-facing configuration ISO URL")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *input == "" || flags.NArg() != 0 {
			return errors.New("check-media requires --input, --boot-url, and --data-url")
		}
		content, err := os.ReadFile(*input) // #nosec G304 G703 -- Explicit local CLI input, never a remote request.
		if err != nil {
			return err
		}
		result, err := checkMedia(content, *boot, *data)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(result)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// Preserve the complete embedded Ignition, including SSH access, and replace
// only the known preparation unit. An unexpected layout needs investigation.
func maskPreparation(content []byte) ([]byte, error) {
	var config map[string]json.RawMessage
	if err := json.Unmarshal(content, &config); err != nil {
		return nil, errors.New("input is not an Ignition JSON object")
	}
	var ignition struct {
		Version string `json:"version"`
		Config  struct {
			Merge   []json.RawMessage `json:"merge"`
			Replace *struct {
				Source *string `json:"source"`
			} `json:"replace"`
		} `json:"config"`
	}
	if err := json.Unmarshal(config["ignition"], &ignition); err != nil ||
		!regexp.MustCompile(`^3\.\d+\.\d+$`).MatchString(ignition.Version) {
		return nil, errors.New("expected complete Ignition version 3 configuration")
	}
	if len(ignition.Config.Merge) != 0 ||
		(ignition.Config.Replace != nil && ignition.Config.Replace.Source != nil) {
		return nil, errors.New("remote Ignition merge/replace references require inspection before masking preparation")
	}
	var systemd map[string]json.RawMessage
	if err := json.Unmarshal(config["systemd"], &systemd); err != nil {
		return nil, errors.New("input has no systemd configuration")
	}
	var units []map[string]json.RawMessage
	if err := json.Unmarshal(systemd["units"], &units); err != nil {
		return nil, errors.New("input has no systemd units")
	}
	found := 0
	for i, unit := range units {
		var name string
		if err := json.Unmarshal(unit["name"], &name); err != nil {
			return nil, errors.New("systemd unit has an invalid name")
		}
		if name == preparationService {
			found++
			units[i] = map[string]json.RawMessage{"name": unit["name"], "mask": json.RawMessage("true")}
		}
	}
	if found != 1 {
		return nil, fmt.Errorf("expected exactly one %s unit, found %d; inspect this ISO before proceeding", preparationService, found)
	}
	// All values below consist of already validated JSON, so marshaling cannot fail.
	systemd["units"], _ = json.Marshal(units)
	config["systemd"], _ = json.Marshal(systemd)
	return json.MarshalIndent(config, "", "  ")
}

// Create only harmless diagnostic contents. This is deliberately not a complete
// installation configuration ISO and must not be used for disk-boot handoff.
func createProbeISO(output, marker string) error {
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`).MatchString(marker) {
		return errors.New("marker must be 1–128 letters, digits, dots, underscores, or hyphens, starting with a letter or digit")
	}
	work, err := os.MkdirTemp("", "ibi-media-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	tree := filepath.Join(work, "contents")
	if err = os.MkdirAll(filepath.Join(tree, "cluster-configuration"), 0700); err != nil {
		return err
	}
	// Work is a private MkdirTemp directory; this filename is fixed.
	if err = os.WriteFile(filepath.Join(tree, "ibi-media-probe.txt"), []byte(marker+"\n"), 0600); err != nil { // #nosec G703
		return err
	}
	manifest := []byte("{\"api_version\":1,\"raw_nm_state_config\":\"\"}\n")
	if err = os.WriteFile(filepath.Join(tree, "cluster-configuration", "manifest.json"), manifest, 0600); err != nil {
		return err
	}
	path := filepath.Join(work, "configuration.iso")
	if err = buildISO(path, tree); err != nil {
		return err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return writeNew(output, content)
}

// Use the same ISO9660 backend, Rock Ridge option, and volume label as the
// existing installer configuration ISO generator, without installing tools.
func buildISO(output, tree string) error {
	d, err := diskfs.Create(output, 38*1024, diskfs.Raw, diskfs.SectorSizeDefault)
	if err != nil {
		return err
	}
	defer d.File.Close()
	d.LogicalBlocksize = 2048
	fs, err := d.CreateFilesystem(disk.FilesystemSpec{
		Partition: 0, FSType: filesystem.TypeISO9660, VolumeLabel: "cluster-config", WorkDir: tree,
	})
	if err != nil {
		return err
	}
	iso, ok := fs.(*iso9660.FileSystem)
	if !ok {
		return errors.New("expected ISO9660 filesystem")
	}
	return iso.Finalize(iso9660.FinalizeOptions{RockRidge: true, VolumeIdentifier: "cluster-config"})
}

func writeNew(path string, content []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600) // #nosec G304 G703 -- Explicit local output; O_EXCL prevents replacement/symlink following.
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		_ = os.Remove(path) // #nosec G703 -- Remove only this CLI invocation's failed O_EXCL-created output.
		return err
	}
	return f.Close()
}

type virtualMedia struct {
	Resource string `json:"@odata.id"`
	Inserted *bool  `json:"Inserted"`
	Image    string `json:"Image"`
}

type mediaResult struct {
	BootDevice              string `json:"bootDevice"`
	DataDevice              string `json:"dataDevice"`
	GuestVisibilityVerified bool   `json:"guestVisibilityVerified"`
	ISOBytesVerified        bool   `json:"isoBytesVerified"`
}

func checkMedia(content []byte, bootURL, dataURL string) (*mediaResult, error) {
	for _, value := range []string{bootURL, dataURL} {
		u, err := url.Parse(value)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, errors.New("both expected BMC-facing URLs must be absolute HTTP(S) URLs")
		}
	}
	if bootURL == dataURL {
		return nil, errors.New("boot and configuration media must have distinct URLs")
	}
	var members []virtualMedia
	if err := json.Unmarshal(content, &members); err != nil {
		var collection struct {
			Members []virtualMedia `json:"Members"`
		}
		if err := json.Unmarshal(content, &collection); err != nil {
			return nil, errors.New("expected an expanded Redfish collection or array")
		}
		members = collection.Members
	}
	result := &mediaResult{}
	seen := make(map[string]bool)
	for _, member := range members {
		if member.Resource == "" || member.Inserted == nil {
			return nil, errors.New("each member needs @odata.id and Inserted; fetch individual resources when collection expansion is unsupported")
		}
		if seen[member.Resource] {
			return nil, errors.New("snapshot repeats a virtual-media resource")
		}
		seen[member.Resource] = true
		if *member.Inserted && member.Image == bootURL {
			result.BootDevice = member.Resource
		}
		if *member.Inserted && member.Image == dataURL {
			result.DataDevice = member.Resource
		}
	}
	if result.BootDevice == "" || result.DataDevice == "" {
		return nil, errors.New("snapshot does not show both expected images inserted in distinct devices; DataImage status alone is insufficient")
	}
	return result, nil
}
