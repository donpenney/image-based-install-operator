// Copyright 2026 Red Hat, Inc.
// SPDX-License-Identifier: Apache-2.0

// preparation-ignition adds generic preparation support to a complete live ISO
// Ignition read from stdin. It writes the updated Ignition to stdout.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/openshift/image-based-install-operator/internal/preparation"
)

func run(input io.Reader, output io.Writer) error {
	content, err := io.ReadAll(io.LimitReader(input, 16*1024*1024+1))
	if err == nil && len(content) > 16*1024*1024 {
		err = fmt.Errorf("Ignition exceeds 16 MiB")
	}
	if err == nil {
		content, err = preparation.AddIgnition(content)
	}
	if err == nil {
		_, err = output.Write(append(content, '\n'))
	}
	return err
}
func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
