// Copyright 2026 Red Hat, Inc.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"fmt"
	"net/url"
	"regexp"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type PreProvisioningSpec struct {
	// ISOURL is an immutable HTTPS IBI live ISO on an operator-allowed origin.
	ISOURL string `json:"isoURL"`
	// ISODigest is the SHA-256 digest of the exact ISO bytes.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	ISODigest string `json:"isoDigest"`
	// ISOServerCACertRef identifies a ConfigMap containing ca-bundle.crt.
	// +optional
	ISOServerCACertRef *corev1.LocalObjectReference `json:"isoServerCACertRef,omitempty"`
	// Timeout bounds each preparation attempt, including artifact preparation.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
	// MaxRetries is the number of additional attempts after the first.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=10
	MaxRetries int `json:"maxRetries,omitempty"`
	// RetryBackoff is applied only after the previous deployment is torn down.
	// +optional
	RetryBackoff *metav1.Duration `json:"retryBackoff,omitempty"`
}

type PreProvisioningStatus struct {
	Phase           string                 `json:"phase"`
	AttemptID       string                 `json:"attemptID"`
	Attempt         int                    `json:"attempt"`
	HostRef         BareMetalHostReference `json:"hostRef"`
	HostUID         types.UID              `json:"hostUID"`
	BMCAddress      string                 `json:"bmcAddress"`
	BootMACAddress  string                 `json:"bootMACAddress"`
	BootURL         string                 `json:"bootURL"`
	ConfigURL       string                 `json:"configURL"`
	StartedAt       metav1.Time            `json:"startedAt"`
	Deadline        metav1.Time            `json:"deadline"`
	OverallDeadline metav1.Time            `json:"overallDeadline"`
	// TokenHash authenticates callbacks without putting the token in status.
	// +optional
	TokenHash string `json:"tokenHash,omitempty"`
	// Result is an immutable callback result for this attempt.
	// +optional
	Result string `json:"result,omitempty"`
	// Message contains a bounded diagnostic without credentials.
	// +optional
	Message string `json:"message,omitempty"`
	// RetryAt is recorded after safe teardown of a failed attempt.
	// +optional
	RetryAt *metav1.Time `json:"retryAt,omitempty"`
	// DeploymentReleased is the persisted media-teardown checkpoint. Artifact
	// cleanup and handoff can still be in progress.
	// +optional
	DeploymentReleased bool `json:"deploymentReleased,omitempty"`
}

const (
	PreparationArtifacts = "PreparingArtifacts"
	PreparationBooting   = "Booting"
	PreparationStopping  = "Stopping"
	PreparationRetry     = "RetryBackoff"
	PreparationComplete  = "Completed"
	PreparationFailed    = "Failed"
)

func (p *PreProvisioningSpec) AttemptTimeout() time.Duration {
	if p != nil && p.Timeout != nil {
		return p.Timeout.Duration
	}
	return time.Hour
}
func (p *PreProvisioningSpec) Backoff() time.Duration {
	if p != nil && p.RetryBackoff != nil {
		return p.RetryBackoff.Duration
	}
	return 5 * time.Minute
}
func (p *PreProvisioningSpec) Validate() error {
	if p == nil {
		return nil
	}
	u, err := url.Parse(p.ISOURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("preProvisioning.isoURL must be an absolute HTTPS URL without credentials or a fragment")
	}
	if !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(p.ISODigest) {
		return fmt.Errorf("preProvisioning.isoDigest must be a SHA-256 digest")
	}
	if p.AttemptTimeout() <= 0 || p.AttemptTimeout() > 24*time.Hour || p.Backoff() < 0 || p.Backoff() > 24*time.Hour || p.MaxRetries < 0 || p.MaxRetries > 10 {
		return fmt.Errorf("preProvisioning requires a timeout in (0,24h], retry backoff in [0,24h], and 0..10 retries")
	}
	if p.ISOServerCACertRef != nil && p.ISOServerCACertRef.Name == "" {
		return fmt.Errorf("preProvisioning.isoServerCACertRef.name must not be empty")
	}
	return nil
}
