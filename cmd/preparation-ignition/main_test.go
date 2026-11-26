// Copyright 2026 Red Hat, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunRejectsInvalidInputWithoutPartialOutput(t *testing.T) {
	for _, input := range []string{"invalid JSON", strings.Repeat(" ", 16*1024*1024+1)} {
		var output bytes.Buffer
		if err := run(strings.NewReader(input), &output); err == nil {
			t.Fatal("accepted invalid or oversized Ignition")
		}
		if output.Len() != 0 {
			t.Fatal("emitted partial configuration after validation failed")
		}
	}
}
func TestRunAddsPreparationToExistingIgnition(t *testing.T) {
	input := `{"ignition":{"version":"3.2.0"},"systemd":{"units":[{"name":"install-rhcos-and-restore-seed.service","contents":"[Service]\nType=oneshot\nExecStart=/prepare"}]}}`
	var output bytes.Buffer
	if err := run(strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "ibi-preparation-bootstrap.service") {
		t.Fatal("did not produce preparation Ignition")
	}
}
