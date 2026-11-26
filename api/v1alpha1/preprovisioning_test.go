// Copyright 2026 Red Hat, Inc.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPreProvisioningValidation(t *testing.T) {
	valid := PreProvisioningSpec{ISOURL: "https://iso.example.com/live.iso", ISODigest: "sha256:" + strings.Repeat("a", 64)}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*PreProvisioningSpec)
	}{
		{"missing digest", func(p *PreProvisioningSpec) { p.ISODigest = "" }},
		{"HTTP URL", func(p *PreProvisioningSpec) { p.ISOURL = "http://iso.example.com/live.iso" }},
		{"URL credentials", func(p *PreProvisioningSpec) { p.ISOURL = "https://user:password@iso.example.com/live.iso" }},
		{"negative retries", func(p *PreProvisioningSpec) { p.MaxRetries = -1 }},
		{"unbounded retries", func(p *PreProvisioningSpec) { p.MaxRetries = 11 }},
		{"zero timeout", func(p *PreProvisioningSpec) { p.Timeout = &metav1.Duration{} }},
		{"negative backoff", func(p *PreProvisioningSpec) { p.RetryBackoff = &metav1.Duration{Duration: -time.Second} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := valid
			tc.change(&copy)
			if err := copy.Validate(); err == nil {
				t.Fatal("accepted invalid preparation inputs")
			}
		})
	}
}
func TestPreProvisioningFreezesSpecBeforeNormalBoot(t *testing.T) {
	old := &ImageClusterInstall{Spec: ImageClusterInstallSpec{PreProvisioning: &PreProvisioningSpec{ISOURL: "https://iso.example.com/first.iso", ISODigest: "sha256:" + strings.Repeat("a", 64)}}, Status: ImageClusterInstallStatus{PreProvisioning: &PreProvisioningStatus{Phase: PreparationArtifacts}}}
	updated := old.DeepCopy()
	updated.Spec.PreProvisioning.ISOURL = "https://iso.example.com/second.iso"
	if old.Spec.PreProvisioning.ISOURL == updated.Spec.PreProvisioning.ISOURL {
		t.Fatal("deepcopy did not isolate the preparation configuration")
	}
	if _, err := updated.ValidateUpdate(old); err == nil {
		t.Fatal("allowed changing the claimed preparation configuration")
	}
	updated = old.DeepCopy()
	updated.Spec.PreProvisioning = nil
	if _, err := updated.ValidateUpdate(old); err == nil {
		t.Fatal("allowed disabling preparation after an attempt started")
	}
}
