// Copyright 2026 Red Hat, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ignition "github.com/coreos/ignition/v2/config/v3_2/types"
	diskfs "github.com/diskfs/go-diskfs"
)

const liveIgnition = `{
 "ignition":{"version":"3.2.0"},
 "passwd":{"users":[{"name":"core","sshAuthorizedKeys":["original-key"]}]},
 "storage":{"files":[{"path":"/etc/probe.txt","mode":420,"contents":{"source":"data:,unchanged"}}]},
 "systemd":{"units":[
  {"name":"install-rhcos-and-restore-seed.service","enabled":true,"contents":"[Service]\nExecStart=/usr/local/bin/prepare\n","dropins":[{"name":"timeout.conf","contents":"[Service]\nTimeoutStartSec=600\n"}]},
  {"name":"sshd.service","enabled":true}
 ]}
}`

func TestMaskPreparationPreservesOtherConfiguration(t *testing.T) {
	masked, err := maskPreparation([]byte(liveIgnition))
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]json.RawMessage
	if err := json.Unmarshal([]byte(liveIgnition), &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(masked, &after); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ignition", "storage", "passwd"} {
		var compactBefore, compactAfter bytes.Buffer
		if err := json.Compact(&compactBefore, before[key]); err != nil {
			t.Fatal(err)
		}
		if err := json.Compact(&compactAfter, after[key]); err != nil {
			t.Fatal(err)
		}
		if compactBefore.String() != compactAfter.String() {
			t.Fatalf("changed unrelated %s configuration", key)
		}
	}
	var config ignition.Config
	if err := json.Unmarshal(masked, &config); err != nil {
		t.Fatalf("invalid diagnostic Ignition: %v", err)
	}
	prep := config.Systemd.Units[0]
	if prep.Mask == nil || !*prep.Mask || prep.Enabled != nil || prep.Contents != nil || len(prep.Dropins) != 0 {
		t.Fatalf("preparation unit is not exclusively masked: %+v", prep)
	}
	if other := config.Systemd.Units[1]; other.Name != "sshd.service" || other.Enabled == nil || !*other.Enabled {
		t.Fatal("changed the SSH service")
	}
}

// The installer serializes empty nested structs as replace.verification even
// when no replacement source exists. Such output is complete embedded Ignition.
func TestMaskPreparationAcceptsSerializedInstallerIgnition(t *testing.T) {
	var original ignition.Config
	if err := json.Unmarshal([]byte(liveIgnition), &original); err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	masked, err := maskPreparation(content)
	if err != nil {
		t.Fatalf("rejected installer-serialized Ignition without remote references: %v", err)
	}
	var result ignition.Config
	if err := json.Unmarshal(masked, &result); err != nil {
		t.Fatal(err)
	}
	if unit := result.Systemd.Units[0]; unit.Mask == nil || !*unit.Mask || unit.Enabled != nil || unit.Contents != nil {
		t.Fatal("installer-serialized preparation unit was not masked")
	}
	if got := result.Passwd.Users[0].SSHAuthorizedKeys[0]; got != original.Passwd.Users[0].SSHAuthorizedKeys[0] {
		t.Fatal("changed the embedded SSH key")
	}
}

func TestMaskPreparationRejectsUnexpectedLayouts(t *testing.T) {
	cases := []string{
		`null`, `[]`, `{}`, `{"ignition":{"version":"2.2.0"}}`,
		strings.Replace(liveIgnition, preparationService, "different-preparation.service", 1),
		strings.Replace(liveIgnition, "sshd.service", preparationService, 1),
		strings.Replace(liveIgnition, `"sshd.service"`, `42`, 1),
		strings.Replace(liveIgnition, `"version":"3.2.0"`, `"version":"3.2.0","config":{"merge":[{"source":"https://example.com/other.ign"}]}`, 1),
		strings.Replace(liveIgnition, `"version":"3.2.0"`, `"version":"3.2.0","config":{"replace":{"source":"https://example.com/other.ign"}}`, 1),
	}
	for _, input := range cases {
		if _, err := maskPreparation([]byte(input)); err == nil {
			t.Errorf("accepted an unexpected Ignition layout: %s", input)
		}
	}
}

func TestConfigISOHasExpectedVolumeAndDiagnosticContents(t *testing.T) {
	output := filepath.Join(t.TempDir(), "configuration.iso")
	if err := createProbeISO(output, "attempt-1"); err != nil {
		t.Fatal(err)
	}
	d, err := diskfs.Open(output, diskfs.WithOpenMode(diskfs.ReadOnly))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.File.Close() })
	fs, err := d.GetFilesystem(0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimRight(fs.Label(), "\x00 ") != "cluster-config" {
		t.Fatalf("unexpected volume label %q", fs.Label())
	}
	for path, expected := range map[string]string{
		"/ibi-media-probe.txt":                 "attempt-1\n",
		"/cluster-configuration/manifest.json": "{\"api_version\":1,\"raw_nm_state_config\":\"\"}\n",
	} {
		f, err := fs.OpenFile(path, os.O_RDONLY)
		if err != nil {
			t.Fatal(err)
		}
		content, readErr := io.ReadAll(f)
		_ = f.Close()
		if readErr != nil || string(content) != expected {
			t.Fatalf("unexpected %s contents: %q (%v)", path, content, readErr)
		}
	}
	if err := createProbeISO(output, "attempt-2"); err == nil {
		t.Fatal("overwrote an existing ISO")
	}
}

func TestConfigISORejectsInvalidMarkers(t *testing.T) {
	for _, marker := range []string{"", "../other", "a\nb", strings.Repeat("x", 129)} {
		if err := createProbeISO(filepath.Join(t.TempDir(), "probe.iso"), marker); err == nil {
			t.Errorf("accepted marker %q", marker)
		}
	}
}

func TestNewOutputsDoNotClobberFilesOrSymlinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing")
	if err := writeNew(path, []byte("keep")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{path, link} {
		if err := writeNew(output, []byte("replace")); err == nil {
			t.Fatal("replaced an existing output")
		}
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "keep" {
		t.Fatalf("modified original output: %q (%v)", content, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("output must be private: %v (%v)", info, err)
	}
}

func TestMediaRequiresDistinctInsertedExpectedImages(t *testing.T) {
	const boot, data = "http://ironic.example/boot.iso", "http://ironic.example/config.iso"
	const members = `[{"@odata.id":"/redfish/v1/Managers/1/VirtualMedia/CD","Inserted":true,"Image":"http://ironic.example/boot.iso"},{"@odata.id":"/redfish/v1/Managers/1/VirtualMedia/DVD","Inserted":true,"Image":"http://ironic.example/config.iso"}]`
	for _, content := range []string{members, `{"Members":` + members + `}`} {
		result, err := checkMedia([]byte(content), boot, data)
		if err != nil {
			t.Fatal(err)
		}
		if result.BootDevice == result.DataDevice || result.GuestVisibilityVerified || result.ISOBytesVerified {
			t.Fatalf("overstated evidence: %+v", result)
		}
	}
	cases := map[string]string{
		"no media":         `[]`,
		"invalid JSON":     `bad`,
		"unexpanded links": `{"Members":[{"@odata.id":"/redfish/v1/Managers/1/VirtualMedia/CD"}]}`,
		"not inserted":     strings.Replace(members, `"Inserted":true`, `"Inserted":false`, 1),
		"different image":  strings.Replace(members, "config.iso", "other.iso", 1),
		"duplicate device": strings.Replace(members, "/VirtualMedia/DVD", "/VirtualMedia/CD", 1),
		"no resource ID":   strings.Replace(members, `"@odata.id"`, `"unrelated"`, 1),
		"status only":      `{"status":{"attachedImage":{"url":"http://ironic.example/config.iso"}}}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := checkMedia([]byte(input), boot, data); err == nil {
				t.Fatal("accepted incomplete media evidence")
			}
		})
	}
	for _, pair := range [][2]string{{boot, boot}, {"", data}, {"/boot.iso", data}, {"file:///boot.iso", data}} {
		if _, err := checkMedia([]byte(members), pair[0], pair[1]); err == nil {
			t.Errorf("accepted invalid expected URLs: %v", pair)
		}
	}
}

func TestCLIRejectsIncompleteArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"unknown"}, {"config-iso"}, {"mask-ignition"}, {"check-media"}, {"config-iso", "--unexpected"}} {
		if err := run(args, io.Discard); err == nil {
			t.Errorf("accepted incomplete arguments: %v", args)
		}
	}
}

func TestCLIProducesMaskedIgnitionAndMediaEvidence(t *testing.T) {
	dir := t.TempDir()
	input, output := filepath.Join(dir, "original.ign"), filepath.Join(dir, "masked.ign")
	if err := os.WriteFile(input, []byte(liveIgnition), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"mask-ignition", "--input", input, "--output", output}, io.Discard); err != nil {
		t.Fatal(err)
	}
	var config ignition.Config
	masked, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(masked, &config); err != nil || config.Systemd.Units[0].Mask == nil || !*config.Systemd.Units[0].Mask {
		t.Fatalf("CLI did not mask the requested ISO configuration: %v", err)
	}
	iso := filepath.Join(dir, "diagnostic.iso")
	if err := run([]string{"config-iso", "--output", iso, "--marker", "cli-test"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(dir, "media.json")
	media := `[{"@odata.id":"/redfish/v1/Managers/1/VirtualMedia/CD","Inserted":true,"Image":"http://example.com/boot.iso?private=boot"},{"@odata.id":"/redfish/v1/Managers/1/VirtualMedia/DVD","Inserted":true,"Image":"http://example.com/data.iso?private=data"}]`
	if err := os.WriteFile(snapshot, []byte(media), 0600); err != nil {
		t.Fatal(err)
	}
	var result bytes.Buffer
	if err := run([]string{"check-media", "--input", snapshot, "--boot-url", "http://example.com/boot.iso?private=boot", "--data-url", "http://example.com/data.iso?private=data"}, &result); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(result.String(), "private=") || strings.Contains(result.String(), "http://") {
		t.Fatal("CLI leaked media URLs into its result")
	}
	var evidence mediaResult
	if err := json.Unmarshal(result.Bytes(), &evidence); err != nil || evidence.BootDevice == "" || evidence.DataDevice == "" {
		t.Fatalf("missing media evidence: %v", err)
	}
}
