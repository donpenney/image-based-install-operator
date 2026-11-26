// Copyright 2026 Red Hat, Inc.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCollectorUsesExplicitContextAndPrivateNewArtifacts(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("collector integration test requires jq")
	}
	dir := t.TempDir()
	callLog := filepath.Join(dir, "calls")
	fakeOC := `#!/bin/bash
set -eu
printf '%s\n' "$*" >> "$IBI_PROBE_CALLS"
case " $* " in
    *" get "*) ;;
    *) exit 99 ;;
esac
case " $* " in
    *" dataimages "*) exit 0 ;;
    *" deployments "*|*" events "*) echo '{"items":[]}' ;;
    *) echo '{"metadata":{"name":"probe","namespace":"probe","uid":"uid"},"spec":{"online":false},"status":{"desired":{"version":"4.test"}}}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "oc"), []byte(fakeOC), 0700); err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(dir, "explicit-kubeconfig")
	if err := os.WriteFile(kubeconfig, nil, 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "artifacts")
	invoke := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "bash", "collect.sh", kubeconfig, "probe-namespace", "probe-host", output)
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "IBI_PROBE_CALLS="+callLog)
		if b, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%w: %s", err, b)
		}
		return nil
	}
	if err := invoke(); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if !strings.Contains(call, "--kubeconfig="+kubeconfig+" --request-timeout=30s get ") {
			t.Fatalf("unexpected operation or implicit context: %s", call)
		}
	}
	entries, err := os.ReadDir(output)
	if err != nil || len(entries) != 6 {
		t.Fatalf("expected complete evidence collection: %v (%v)", entries, err)
	}
	for _, entry := range entries {
		info, infoErr := entry.Info()
		if infoErr != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("artifact permissions: %v (%v)", info, infoErr)
		}
	}
	missing, err := os.ReadFile(filepath.Join(output, "dataimage.json"))
	if err != nil || !bytes.Contains(missing, []byte(`"present":false`)) {
		t.Fatalf("absent DataImage was not recorded: %q (%v)", missing, err)
	}
	if invokeErr := invoke(); invokeErr == nil {
		t.Fatal("reused an existing collection directory")
	}
	after, err := os.ReadFile(callLog)
	if err != nil || !bytes.Equal(calls, after) {
		t.Fatal("issued API reads after detecting existing artifacts")
	}
}
