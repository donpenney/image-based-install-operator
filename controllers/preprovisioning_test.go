// Copyright 2026 Red Hat, Inc.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bmhv1 "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	"github.com/openshift/image-based-install-operator/api/v1alpha1"
	"github.com/openshift/image-based-install-operator/internal/credentials"
	"github.com/openshift/image-based-install-operator/internal/preparation"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type preparationFixture struct {
	client  client.Client
	cache   client.Client
	ici     *v1alpha1.ImageClusterInstall
	host    *bmhv1.BareMetalHost
	options *ImageClusterInstallReconcilerOptions
	scheme  *runtime.Scheme
}

func newPreparationFixture(t *testing.T) *preparationFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{v1alpha1.AddToScheme, bmhv1.AddToScheme, corev1.AddToScheme, hivev1.AddToScheme} {
		if err := register(scheme); err != nil {
			t.Fatal(err)
		}
	}
	host := &bmhv1.BareMetalHost{ObjectMeta: metav1.ObjectMeta{Name: "server", Namespace: "inventory", UID: "host-uid"}, Spec: bmhv1.BareMetalHostSpec{ExternallyProvisioned: true, AutomatedCleaningMode: bmhv1.CleaningModeDisabled, PreprovisioningNetworkDataName: "network-data"}}
	host.Status.Provisioning.State = bmhv1.StateAvailable
	ici := &v1alpha1.ImageClusterInstall{ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "target", UID: "00000000-0000-0000-0000-000000000001", Finalizers: []string{clusterInstallFinalizerName}}, Spec: v1alpha1.ImageClusterInstallSpec{BareMetalHostRef: &v1alpha1.BareMetalHostReference{Name: host.Name, Namespace: host.Namespace}, PreProvisioning: &v1alpha1.PreProvisioningSpec{ISOURL: "https://iso.example.com/live.iso", ISODigest: "sha256:" + strings.Repeat("a", 64)}}}
	network := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "network-data", Namespace: host.Namespace}, Data: map[string][]byte{nmstateSecretKey: []byte("interfaces: []\n")}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(ici, host, &bmhv1.DataImage{}).WithObjects(ici, host, network).Build()
	return &preparationFixture{client: c, ici: ici, host: host, scheme: scheme, options: &ImageClusterInstallReconcilerOptions{DataDir: t.TempDir(), PreparationBaseURL: "https://preparation.example.com", PreparationISOOrigins: "https://iso.example.com"}}
}
func (f *preparationFixture) reload(t *testing.T) {
	t.Helper()
	if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(f.ici), f.ici); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(f.host), f.host); err != nil {
		t.Fatal(err)
	}
}
func (f *preparationFixture) reconciler() *ImageClusterInstallReconciler {
	cachedClient := f.cache
	if cachedClient == nil {
		cachedClient = f.client
	}
	return &ImageClusterInstallReconciler{Client: cachedClient, NoncachedClient: f.client, Scheme: f.scheme, Log: logrus.New(), Options: f.options, BaseURL: "https://config.internal.example.com:8000"}
}

// Match the Secret selector configured by cmd/manager: writes reach the API,
// while subsequent cached reads can see only operator-labelled Secrets.
type preparationSecretCache struct{ client.Client }

func (c preparationSecretCache) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := c.Client.Get(ctx, key, obj, opts...); err != nil {
		return err
	}
	if _, isSecret := obj.(*corev1.Secret); isSecret && obj.GetLabels()[credentials.SecretResourceLabel] != credentials.SecretResourceValue {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, key.Name)
	}
	return nil
}

func TestPreparationArtifactsRemainVisibleThroughManagerSecretCache(t *testing.T) {
	f := newPreparationFixture(t)
	f.cache = preparationSecretCache{Client: f.client}
	f.boot(t)
	// A fresh reconciler must adopt both immutable Secrets from the filtered
	// cache, including after the attempt moved beyond artifact creation.
	f.reload(t)
	r := f.reconciler()
	if _, err := r.preparationInputs(t.Context(), f.ici, f.host); err != nil {
		t.Fatalf("input snapshot is invisible through the manager cache: %v", err)
	}
	if _, err := r.ensurePreparationArtifact(t.Context(), f.ici, f.host); err != nil {
		t.Fatalf("attempt artifact is invisible through the manager cache: %v", err)
	}
}
func (f *preparationFixture) tick(t *testing.T) bool {
	t.Helper()
	f.reload(t)
	stop, _, err := f.reconciler().reconcilePreparation(t.Context(), f.ici, f.host, &hivev1.ClusterInstallCondition{})
	if err != nil {
		t.Fatal(err)
	}
	f.reload(t)
	return stop
}
func (f *preparationFixture) cacheBoot(t *testing.T) {
	t.Helper()
	// CacheISO has real TLS/digest tests. This fixture starts with its output
	// already verified so controller workflow tests never make network calls.
	path := preparation.BootPath(f.options.DataDir, f.ici.Namespace, string(f.ici.UID), f.ici.Status.PreProvisioning.AttemptID)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("verified test ISO"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".sha256", []byte(f.ici.Spec.PreProvisioning.ISODigest), 0600); err != nil {
		t.Fatal(err)
	}
}
func (f *preparationFixture) boot(t *testing.T) {
	t.Helper()
	f.tick(t)
	f.cacheBoot(t)
	for step := 0; step < 12; step++ {
		f.tick(t)
		image := &bmhv1.DataImage{}
		if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(f.host), image); err == nil && len(image.Finalizers) == 0 {
			image.Finalizers = []string{bmhv1.DataImageFinalizer}
			if err := f.client.Update(t.Context(), image); err != nil {
				t.Fatal(err)
			}
		}
		if f.host.Spec.Online && f.host.Spec.Image != nil {
			f.host.Status.PoweredOn = true
			f.host.Status.Provisioning.State = bmhv1.StateProvisioned
			if err := f.client.Status().Update(t.Context(), f.host); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("preparation did not request live ISO boot")
}
func (f *preparationFixture) callback(t *testing.T, result string) int {
	t.Helper()
	secret := &corev1.Secret{}
	if err := f.client.Get(t.Context(), client.ObjectKey{Namespace: f.ici.Namespace, Name: preparation.SecretName(string(f.ici.UID), f.ici.Status.PreProvisioning.Attempt)}, secret); err != nil {
		t.Fatal(err)
	}
	var payload preparation.Payload
	if err := json.Unmarshal(secret.Data[preparation.PayloadKey], &payload); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(payload.CallbackURL)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, parsed.Path, strings.NewReader(`{"result":"`+result+`","hostUID":"`+string(f.host.UID)+`"}`))
	request.Header.Set("Authorization", "Bearer "+payload.Token)
	response := httptest.NewRecorder()
	(&preparation.Handler{Client: f.client, DataDir: f.options.DataDir}).CallbackMux().ServeHTTP(response, request)
	return response.Code
}
func (f *preparationFixture) acknowledgeTeardown(t *testing.T) {
	t.Helper()
	f.reload(t)
	f.host.Status.PoweredOn = false
	if f.host.Spec.Image == nil {
		f.host.Status.Provisioning.State = bmhv1.StateAvailable
	}
	if err := f.client.Status().Update(t.Context(), f.host); err != nil {
		t.Fatal(err)
	}
	image := &bmhv1.DataImage{}
	if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(f.host), image); err == nil && !image.DeletionTimestamp.IsZero() {
		image.Finalizers = nil
		if err := f.client.Update(t.Context(), image); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPreparationSuccessSurvivesReconcilerRestartAndPreservesDisk(t *testing.T) {
	f := newPreparationFixture(t)
	f.boot(t)
	if f.ici.Status.PreProvisioning.TokenHash == "" || !f.ici.Status.BootTime.IsZero() {
		t.Fatal("normal installation started before preparation completed")
	}
	if code := f.callback(t, "success"); code != 200 {
		t.Fatalf("callback HTTP %d", code)
	}
	f.tick(t) // A fresh reconciler observes the persisted callback.
	if f.ici.Status.PreProvisioning.TokenHash != "" {
		t.Fatal("completed attempt token was not revoked")
	}
	f.tick(t) // Request power-off, retaining both media and source artifacts.
	if f.host.Spec.Online || f.host.Spec.Image == nil {
		t.Fatal("did not power off before removing boot media")
	}
	image := &bmhv1.DataImage{}
	if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(f.host), image); err != nil {
		t.Fatal(err)
	}
	f.acknowledgeTeardown(t)
	f.tick(t) // Clear spec.image only after power-off.
	if f.host.Spec.Image != nil {
		t.Fatal("did not request live deployment teardown")
	}
	f.tick(t) // BMO has not acknowledged available yet.
	if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(f.host), image); err != nil {
		t.Fatal("deleted configuration before deployment teardown")
	}
	for step := 0; step < 15 && f.ici.Status.PreProvisioning.Phase != v1alpha1.PreparationComplete; step++ {
		f.acknowledgeTeardown(t)
		f.tick(t)
	}
	if f.ici.Status.PreProvisioning.Phase != v1alpha1.PreparationComplete {
		t.Fatal("handoff did not complete")
	}
	f.tick(t) // Release the claim after the completed status is durable.
	if !f.host.Spec.ExternallyProvisioned || f.host.Spec.Online || f.host.Spec.Image != nil || f.host.Spec.AutomatedCleaningMode != bmhv1.CleaningModeDisabled {
		t.Fatal("unsafe disk-boot handoff")
	}
	if _, present := f.host.Annotations[preparationOwner]; present {
		t.Fatal("preparation retained its host claim")
	}
	if !f.ici.Status.BootTime.IsZero() {
		t.Fatal("preparation changed normal installation timing")
	}
	if f.tick(t) {
		t.Fatal("normal IBI installation remains gated after preparation")
	}

	// Continue across the preparation/installation boundary. Passing the
	// preparation gate must allow normal configuration to request disk boot.
	configure := func() {
		t.Helper()
		proceed, result, err := f.reconciler().configureHost(t.Context(), f.ici,
			"https://config.internal.example.com/images/normal.iso", f.host,
			&hivev1.ClusterInstallCondition{}, logrus.New())
		if err != nil || !result.IsZero() || !proceed {
			t.Fatalf("normal host configuration did not proceed: result=%v error=%v", result, err)
		}
		f.reload(t)
	}
	configure()
	if !f.host.Spec.Online || !annotationExists(&f.host.ObjectMeta, ibioManagedBMH) || f.ici.Status.BootTime.IsZero() {
		t.Fatal("normal IBI handoff did not request disk boot and start installation timing")
	}
	if f.host.Spec.Image != nil || !f.host.Spec.ExternallyProvisioned {
		t.Fatal("normal disk boot changed the prepared host's provisioning mode")
	}
	version := f.host.ResourceVersion
	configure()
	if f.host.ResourceVersion != version {
		t.Fatal("repeated normal configuration changed the already handed-off host")
	}
}

func TestPreparationRetryGetsNewIdentityAndWaitsForCleanup(t *testing.T) {
	f := newPreparationFixture(t)
	f.ici.Spec.PreProvisioning.MaxRetries = 1
	f.ici.Spec.PreProvisioning.RetryBackoff = &metav1.Duration{Duration: 0}
	if err := f.client.Update(t.Context(), f.ici); err != nil {
		t.Fatal(err)
	}
	f.boot(t)
	oldID := f.ici.Status.PreProvisioning.AttemptID
	oldHash := f.ici.Status.PreProvisioning.TokenHash
	f.ici.Status.PreProvisioning.Deadline = metav1.NewTime(time.Now().Add(-time.Second))
	if err := f.client.Status().Update(t.Context(), f.ici); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if f.ici.Status.PreProvisioning.Phase != v1alpha1.PreparationStopping {
		t.Fatal("timeout did not start cleanup")
	}
	for step := 0; step < 15 && f.ici.Status.PreProvisioning.Phase != v1alpha1.PreparationRetry; step++ {
		f.tick(t)
		f.acknowledgeTeardown(t)
	}
	if f.ici.Status.PreProvisioning.Phase != v1alpha1.PreparationRetry {
		t.Fatal("retry was not scheduled after cleanup")
	}
	f.tick(t)
	if f.ici.Status.PreProvisioning.Attempt != 2 || f.ici.Status.PreProvisioning.AttemptID == oldID {
		t.Fatal("retry reused the previous attempt")
	}
	if f.ici.Status.PreProvisioning.Phase != v1alpha1.PreparationArtifacts {
		t.Fatal("expired previous deadline blocked retry")
	}
	f.cacheBoot(t)
	for step := 0; step < 12 && f.ici.Status.PreProvisioning.Phase != v1alpha1.PreparationBooting; step++ {
		f.tick(t)
		image := &bmhv1.DataImage{}
		if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(f.host), image); err == nil && len(image.Finalizers) == 0 {
			image.Finalizers = []string{bmhv1.DataImageFinalizer}
			if err := f.client.Update(t.Context(), image); err != nil {
				t.Fatal(err)
			}
		}
	}
	if f.ici.Status.PreProvisioning.TokenHash == oldHash || f.ici.Status.PreProvisioning.TokenHash == "" {
		t.Fatal("retry did not rotate credentials")
	}
}

func TestPreparationRejectsChangedPhysicalHostWithoutMutation(t *testing.T) {
	f := newPreparationFixture(t)
	f.boot(t)
	replacement := f.host.DeepCopy()
	replacement.UID = "different-physical-host"
	before := replacement.DeepCopy()
	_, _, err := f.reconciler().reconcilePreparation(t.Context(), f.ici, replacement, &hivev1.ClusterInstallCondition{})
	if err == nil {
		t.Fatal("accepted a different host UID")
	}
	if replacement.Spec.Online != before.Spec.Online || replacement.Spec.Image.URL != before.Spec.Image.URL {
		t.Fatal("mutated the replacement host")
	}
}

func TestPreparationDeletionBeforeClaimDoesNotDeleteForeignDataImage(t *testing.T) {
	f := newPreparationFixture(t)
	f.tick(t)
	image := &bmhv1.DataImage{ObjectMeta: metav1.ObjectMeta{Name: f.host.Name, Namespace: f.host.Namespace}, Spec: bmhv1.DataImageSpec{URL: "https://other.example.com/config.iso"}}
	if err := f.client.Create(t.Context(), image); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Delete(t.Context(), f.ici); err != nil {
		t.Fatal(err)
	}
	for step := 0; step < 5; step++ {
		if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(f.ici), f.ici); apierrors.IsNotFound(err) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		_, _, err := f.reconciler().handleFinalizer(t.Context(), logrus.New(), f.ici)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(image), image); err != nil {
		t.Fatal("preparation finalizer deleted another workflow's DataImage")
	}
	if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(f.host), f.host); err != nil {
		t.Fatal(err)
	}
	if f.host.Spec.Online || f.host.Spec.Image != nil {
		t.Fatal("deletion started or rebooted an unclaimed host")
	}
}

func TestPreparationTerminalTimeoutDoesNotStartNormalInstallation(t *testing.T) {
	f := newPreparationFixture(t)
	f.boot(t)
	f.ici.Status.PreProvisioning.Deadline = metav1.NewTime(time.Now().Add(-time.Second))
	if err := f.client.Status().Update(t.Context(), f.ici); err != nil {
		t.Fatal(err)
	}
	for step := 0; step < 20 && f.ici.Status.PreProvisioning.Phase != v1alpha1.PreparationFailed; step++ {
		f.tick(t)
		f.acknowledgeTeardown(t)
	}
	if f.ici.Status.PreProvisioning.Phase != v1alpha1.PreparationFailed || !f.ici.Status.PreProvisioning.DeploymentReleased {
		t.Fatal("timed-out preparation did not finish safe cleanup")
	}
	if !f.tick(t) || !f.ici.Status.BootTime.IsZero() {
		t.Fatal("normal installation started after preparation failed")
	}
}

func TestPreparationInvalidOriginHasNoHostSideEffects(t *testing.T) {
	f := newPreparationFixture(t)
	f.options.PreparationISOOrigins = "https://other.example.com"
	_, _, err := f.reconciler().reconcilePreparation(t.Context(), f.ici, f.host, &hivev1.ClusterInstallCondition{})
	if err == nil {
		t.Fatal("accepted an unapproved ISO origin")
	}
	if f.ici.Status.PreProvisioning != nil || f.host.Spec.Online || f.host.Annotations[preparationOwner] != "" {
		t.Fatal("invalid inputs changed preparation state")
	}
}

func TestAcceptedPreparationSuccessIsHonoredAfterDeadline(t *testing.T) {
	f := newPreparationFixture(t)
	f.boot(t)
	if code := f.callback(t, "success"); code != 200 {
		t.Fatalf("callback HTTP %d", code)
	}
	f.reload(t)
	f.ici.Status.PreProvisioning.Deadline = metav1.NewTime(time.Now().Add(-time.Second))
	if err := f.client.Status().Update(t.Context(), f.ici); err != nil {
		t.Fatal(err)
	}
	// The callback arrived on time; the manager was delayed or restarting.
	f.tick(t)
	if f.ici.Status.PreProvisioning.Result != "success" || f.ici.Status.PreProvisioning.Phase != v1alpha1.PreparationStopping {
		t.Fatal("overwrote an accepted result with a reconciliation timeout")
	}
}

func TestPreparationResumesHandoffAfterHostUpdate(t *testing.T) {
	f := newPreparationFixture(t)
	f.boot(t)
	if code := f.callback(t, "success"); code != 200 {
		t.Fatalf("callback HTTP %d", code)
	}
	for step := 0; step < 20 && !f.ici.Status.PreProvisioning.DeploymentReleased; step++ {
		f.tick(t)
		f.acknowledgeTeardown(t)
	}
	if !f.ici.Status.PreProvisioning.DeploymentReleased {
		t.Fatal("no durable teardown acknowledgement")
	}
	// Simulate interruption after the BMH handoff patch but before the ICI
	// completed status is persisted. The ownership claim must still exist.
	f.host.Spec.ExternallyProvisioned = true
	if err := f.client.Update(t.Context(), f.host); err != nil {
		t.Fatal(err)
	}
	for step := 0; step < 10 && f.ici.Status.PreProvisioning.Phase != v1alpha1.PreparationComplete; step++ {
		f.tick(t)
		if !f.host.Spec.ExternallyProvisioned {
			t.Fatal("restarted deprovisioning during handoff recovery")
		}
	}
	if f.ici.Status.PreProvisioning.Phase != v1alpha1.PreparationComplete {
		t.Fatal("handoff did not recover")
	}
	f.tick(t)
	if f.host.Annotations[preparationOwner] != "" {
		t.Fatal("completed handoff did not release its claim")
	}
}

func TestPreparationValidationRejectsChangedTargetBeforeUpdatingHost(t *testing.T) {
	for _, change := range []string{"uid", "bmc", "bootMAC"} {
		t.Run(change, func(t *testing.T) {
			f := newPreparationFixture(t)
			f.tick(t)
			switch change {
			case "uid":
				f.host.UID = "replacement-host"
			case "bmc":
				f.host.Spec.BMC.Address = "redfish-virtualmedia+https://replacement.example.com/redfish/v1/Systems/1"
			case "bootMAC":
				f.host.Spec.BootMACAddress = "00:11:22:33:44:55"
			}
			f.host.Spec.AutomatedCleaningMode = bmhv1.CleaningModeMetadata
			if err := f.client.Update(t.Context(), f.host); err != nil {
				t.Fatal(err)
			}
			condition := &hivev1.ClusterInstallCondition{}
			_, err := f.reconciler().validateConfiguration(t.Context(), f.ici, &hivev1.ClusterDeployment{}, condition, logrus.New())
			if err == nil {
				t.Fatal("accepted a different preparation target")
			}
			f.reload(t)
			if f.host.Spec.AutomatedCleaningMode != bmhv1.CleaningModeMetadata {
				t.Fatal("modified the replacement target before rejecting it")
			}
		})
	}
}

func TestPreparationDeletionReleasesCompletedHandoffClaim(t *testing.T) {
	f := newPreparationFixture(t)
	f.boot(t)
	if code := f.callback(t, "success"); code != 200 {
		t.Fatalf("callback HTTP %d", code)
	}
	for step := 0; step < 20 && f.ici.Status.PreProvisioning.Phase != v1alpha1.PreparationComplete; step++ {
		f.tick(t)
		f.acknowledgeTeardown(t)
	}
	if f.ici.Status.PreProvisioning.Phase != v1alpha1.PreparationComplete || f.host.Annotations[preparationOwner] == "" {
		t.Fatal("did not reach the persisted-completion handoff window")
	}
	if err := f.client.Delete(t.Context(), f.ici); err != nil {
		t.Fatal(err)
	}
	for step := 0; step < 5; step++ {
		if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(f.ici), f.ici); apierrors.IsNotFound(err) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.reconciler().handleFinalizer(t.Context(), logrus.New(), f.ici); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(f.host), f.host); err != nil {
		t.Fatal(err)
	}
	if f.host.Annotations[preparationOwner] != "" || f.host.Spec.Online || f.host.Spec.Image != nil || !f.host.Spec.ExternallyProvisioned {
		t.Fatal("deletion leaked a preparation claim or changed the prepared host")
	}
	if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(f.ici), f.ici); !apierrors.IsNotFound(err) {
		t.Fatal("completed preparation blocked ICI deletion")
	}
}

func TestPreparationRejectsExistingConfigurationBeforeClaimingHost(t *testing.T) {
	f := newPreparationFixture(t)
	image := &bmhv1.DataImage{ObjectMeta: metav1.ObjectMeta{Name: f.host.Name, Namespace: f.host.Namespace}, Spec: bmhv1.DataImageSpec{URL: "https://other.example.com/config.iso"}}
	if err := f.client.Create(t.Context(), image); err != nil {
		t.Fatal(err)
	}
	condition := &hivev1.ClusterInstallCondition{}
	_, _, err := f.reconciler().reconcilePreparation(t.Context(), f.ici, f.host, condition)
	if err == nil || condition.Reason != "PreProvisioningBlocked" || !strings.Contains(condition.Message, "DataImage") {
		t.Fatal("did not report the existing configuration as a preflight blocker")
	}
	if f.ici.Status.PreProvisioning != nil || f.host.Annotations[preparationOwner] != "" {
		t.Fatal("claimed a host with existing configuration")
	}
}

func TestPreparationRechecksRequiredConfigurationBeforeBoot(t *testing.T) {
	f := newPreparationFixture(t)
	f.boot(t)
	// Simulate the window after the Booting phase is durable but before
	// its first host patch, with the DataImage changed by another writer.
	f.host.Spec.Online = false
	f.host.Spec.Image = nil
	if err := f.client.Update(t.Context(), f.host); err != nil {
		t.Fatal(err)
	}
	image := &bmhv1.DataImage{}
	if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(f.host), image); err != nil {
		t.Fatal(err)
	}
	image.Spec.URL = "https://other.example.com/config.iso"
	if err := f.client.Update(t.Context(), image); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.reconciler().reconcilePreparation(t.Context(), f.ici, f.host, &hivev1.ClusterInstallCondition{})
	if err == nil {
		t.Fatal("booted after the required configuration changed")
	}
	f.reload(t)
	if f.host.Spec.Online || f.host.Spec.Image != nil {
		t.Fatal("requested boot without the expected configuration")
	}
}

func TestPreparationDeletionRevokesCallbackAndWaitsForTeardown(t *testing.T) {
	f := newPreparationFixture(t)
	f.boot(t)
	if err := f.client.Delete(t.Context(), f.ici); err != nil {
		t.Fatal(err)
	}
	f.reload(t)
	if _, _, err := f.reconciler().handleFinalizer(t.Context(), logrus.New(), f.ici); err != nil {
		t.Fatal(err)
	}
	f.reload(t)
	if f.ici.Status.PreProvisioning.TokenHash != "" || f.ici.Status.PreProvisioning.Phase != v1alpha1.PreparationStopping {
		t.Fatal("cancellation did not revoke the attempt before power actions")
	}
	if code := f.callback(t, "success"); code == 200 {
		t.Fatal("accepted completion after cancellation")
	}
	if _, _, err := f.reconciler().handleFinalizer(t.Context(), logrus.New(), f.ici); err != nil {
		t.Fatal(err)
	}
	f.reload(t)
	if f.host.Spec.Online || f.host.Spec.Image == nil || len(f.ici.Finalizers) == 0 {
		t.Fatal("cancellation released media or finalizer before power-off")
	}
	for step := 0; step < 20; step++ {
		if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(f.ici), f.ici); apierrors.IsNotFound(err) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		f.acknowledgeTeardown(t)
		if _, _, err := f.reconciler().handleFinalizer(t.Context(), logrus.New(), f.ici); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(f.ici), f.ici); !apierrors.IsNotFound(err) {
		t.Fatal("cancellation did not complete after deployment teardown")
	}
	if err := f.client.Get(t.Context(), client.ObjectKeyFromObject(f.host), f.host); err != nil {
		t.Fatal(err)
	}
	if f.host.Spec.Online || f.host.Spec.Image != nil || f.host.Annotations[preparationOwner] != "" || f.host.Spec.AutomatedCleaningMode != bmhv1.CleaningModeDisabled {
		t.Fatal("cancellation did not leave the prepared disk offline and released")
	}
	var secrets corev1.SecretList
	if err := f.client.List(t.Context(), &secrets, client.InNamespace(f.ici.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(secrets.Items) != 0 {
		t.Fatal("cancellation left per-attempt credentials behind")
	}
}

func TestPreparationHostReferenceCannotChangeAfterStart(t *testing.T) {
	for _, name := range []string{"not-started", "unchanged", "removed", "different-name", "different-namespace"} {
		t.Run(name, func(t *testing.T) {
			f := newPreparationFixture(t)
			before := f.ici.DeepCopy()
			if name != "not-started" {
				f.ici.Status.PreProvisioning = &v1alpha1.PreProvisioningStatus{HostRef: *f.ici.Spec.BareMetalHostRef}
			}
			switch name {
			case "removed":
				f.ici.Spec.BareMetalHostRef = nil
			case "different-name":
				f.ici.Spec.BareMetalHostRef.Name = "another-host"
			case "different-namespace":
				f.ici.Spec.BareMetalHostRef.Namespace = "another-inventory"
			}
			err := validatePreparationHostReference(f.ici)
			wantError := name == "removed" || name == "different-name" || name == "different-namespace"
			if (err != nil) != wantError {
				t.Fatalf("reference validation error = %v, want rejection %t", err, wantError)
			}
			f.reload(t)
			if *f.ici.Spec.BareMetalHostRef != *before.Spec.BareMetalHostRef {
				t.Fatal("reference validation mutated the stored target")
			}
		})
	}
}

func TestOptionalClusterDeploymentReference(t *testing.T) {
	for _, name := range []string{"absent", "empty", "present", "missing-resource"} {
		t.Run(name, func(t *testing.T) {
			f := newPreparationFixture(t)
			deployment := &hivev1.ClusterDeployment{ObjectMeta: metav1.ObjectMeta{Name: "deployment", Namespace: f.ici.Namespace}}
			switch name {
			case "empty":
				f.ici.Spec.ClusterDeploymentRef = &corev1.LocalObjectReference{}
			case "present", "missing-resource":
				f.ici.Spec.ClusterDeploymentRef = &corev1.LocalObjectReference{Name: deployment.Name}
				if name == "present" {
					if err := f.client.Create(t.Context(), deployment); err != nil {
						t.Fatal(err)
					}
				}
			}
			actual, err := f.reconciler().getCD(t.Context(), f.ici)
			if name == "missing-resource" {
				if !apierrors.IsNotFound(err) {
					t.Fatalf("missing referenced deployment must retain its API error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if name == "present" {
				if actual == nil || actual.UID != deployment.UID {
					t.Fatal("lookup did not return the referenced deployment")
				}
			} else if actual != nil {
				t.Fatal("an absent or empty reference must not resolve a deployment")
			}
		})
	}
}
