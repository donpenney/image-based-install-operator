// Copyright 2026 Red Hat, Inc.
// SPDX-License-Identifier: Apache-2.0

package preparation

import (
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/coreos/go-systemd/v22/unit"
)

//go:embed assets/*
var Assets embed.FS

// AddIgnition extends a complete installer-generated live ISO Ignition with
// generic preparation bootstrap/callback units, preserving its SSH key and
// existing preparation service. Per-host data remains on the configuration ISO.
func AddIgnition(content []byte) ([]byte, error) {
	var config map[string]any
	if err := json.Unmarshal(content, &config); err != nil {
		return nil, err
	}
	ignition, ok := config["ignition"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("complete Ignition is required")
	}
	version, _ := ignition["version"].(string)
	if len(version) < 2 || version[:2] != "3." {
		return nil, fmt.Errorf("Ignition version 3 is required")
	}
	if external, hasExternal := ignition["config"].(map[string]any); hasExternal {
		if merges, hasMerges := external["merge"].([]any); hasMerges && len(merges) != 0 {
			return nil, fmt.Errorf("remote Ignition merges must be resolved before adding preparation units")
		}
		if replace, hasReplace := external["replace"].(map[string]any); hasReplace && replace["source"] != nil {
			return nil, fmt.Errorf("remote Ignition replacement is not supported")
		}
	}
	systemd, ok := config["systemd"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("IBI preparation service is missing")
	}
	units, _ := systemd["units"].([]any)
	found := 0
	for _, value := range units {
		unit, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid Ignition unit")
		}
		name, _ := unit["name"].(string)
		if name == "ibi-preparation-bootstrap.service" || name == "ibi-preparation-failed.service" {
			return nil, fmt.Errorf("preparation units already exist")
		}
		if name != "install-rhcos-and-restore-seed.service" {
			continue
		}
		found++
		if unit["mask"] == true {
			return nil, fmt.Errorf("disk preparation service must not be masked")
		}
		if err := requireOneshot(unit); err != nil {
			return nil, err
		}
		dropins, _ := unit["dropins"].([]any)
		unit["dropins"] = append(dropins, map[string]any{"name": "20-ibi-preparation.conf", "contents": "[Unit]\nRequires=ibi-preparation-bootstrap.service\nAfter=ibi-preparation-bootstrap.service\nOnFailure=ibi-preparation-failed.service\n\n[Service]\nExecStartPost=/usr/local/libexec/ibi-preparation-agent report success\n"})
	}
	if found != 1 {
		return nil, fmt.Errorf("exactly one IBI disk preparation service is required")
	}
	for _, name := range []string{"ibi-preparation-bootstrap.service", "ibi-preparation-failed.service"} {
		contents, err := Assets.ReadFile("assets/" + name)
		if err != nil {
			return nil, err
		}
		units = append(units, map[string]any{"name": name, "contents": string(contents)})
	}
	systemd["units"] = units
	storage, _ := config["storage"].(map[string]any)
	if storage == nil {
		storage = map[string]any{}
		config["storage"] = storage
	}
	files, _ := storage["files"].([]any)
	const path = "/usr/local/libexec/ibi-preparation-agent"
	for _, value := range files {
		if file, ok := value.(map[string]any); ok && file["path"] == path {
			return nil, fmt.Errorf("preparation agent path already exists")
		}
	}
	script, err := Assets.ReadFile("assets/ibi-preparation-agent")
	if err != nil {
		return nil, err
	}
	storage["files"] = append(files, map[string]any{"path": path, "mode": 0755, "contents": map[string]any{"source": "data:;base64," + base64.StdEncoding.EncodeToString(script)}})
	return json.MarshalIndent(config, "", "  ")
}

// ExecStartPost reports completion only for a foreground oneshot command.
// A simple/exec service would report success while preparation was running.
func requireOneshot(service map[string]any) error {
	contents, _ := service["contents"].(string)
	parts := map[string]string{"": contents}
	dropins, _ := service["dropins"].([]any)
	for _, value := range dropins {
		dropin, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("invalid disk preparation drop-in")
		}
		name, _ := dropin["name"].(string)
		if _, duplicate := parts[name]; duplicate || name == "20-ibi-preparation.conf" {
			return fmt.Errorf("duplicate disk preparation drop-in")
		}
		parts[name], _ = dropin["contents"].(string)
	}
	names := make([]string, 0, len(parts))
	for name := range parts {
		names = append(names, name)
	}
	sort.Strings(names)
	serviceType := ""
	for _, name := range names {
		options, err := unit.DeserializeOptions(strings.NewReader(parts[name]))
		if err != nil {
			return fmt.Errorf("invalid disk preparation service: %w", err)
		}
		for _, option := range options {
			if option.Section == "Service" && option.Name == "Type" {
				serviceType = option.Value
			}
		}
	}
	if serviceType != "oneshot" {
		return fmt.Errorf("disk preparation service must use Type=oneshot to report terminal completion")
	}
	return nil
}
