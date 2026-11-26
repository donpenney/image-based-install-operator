// Copyright 2026 Red Hat, Inc.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	bmhv1 "github.com/metal3-io/baremetal-operator/apis/metal3.io/v1alpha1"
	hivev1 "github.com/openshift/hive/apis/hive/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/openshift/image-based-install-operator/api/v1alpha1"
	"github.com/openshift/image-based-install-operator/internal/credentials"
	"github.com/openshift/image-based-install-operator/internal/preparation"
)

const preparationOwner = "imageclusterinstall.extensions.hive.openshift.io/preparation-owner"
const preparationPoll = 5 * time.Second

type preparationInputs struct {
	NMState      string `json:"nmstate"`
	ISOCA        []byte `json:"isoCA,omitempty"`
	CallbackCA   string `json:"callbackCA,omitempty"`
	BaseURL      string `json:"baseURL"`
	ImageBaseURL string `json:"imageBaseURL"`
}

// Validate the reference before following it or touching related resources.
func validatePreparationHostReference(ici *v1alpha1.ImageClusterInstall) error {
	if status := ici.Status.PreProvisioning; status != nil &&
		(ici.Spec.BareMetalHostRef == nil || *ici.Spec.BareMetalHostRef != status.HostRef) {
		return fmt.Errorf("preparation host reference changed after the attempt started")
	}
	return nil
}

func preparationHostMatches(status *v1alpha1.PreProvisioningStatus, host *bmhv1.BareMetalHost) bool {
	return status.HostUID == host.UID && status.HostRef.Name == host.Name && status.HostRef.Namespace == host.Namespace &&
		status.BMCAddress == host.Spec.BMC.Address && status.BootMACAddress == host.Spec.BootMACAddress
}

func inputSecretName(ici *v1alpha1.ImageClusterInstall) string {
	return "ibi-preparation-input-" + string(ici.UID)
}

func (r *ImageClusterInstallReconciler) preparationStatus(ctx context.Context, ici *v1alpha1.ImageClusterInstall) (ctrl.Result, error) {
	s := ici.Status.PreProvisioning
	status := corev1.ConditionFalse
	if s.Phase == v1alpha1.PreparationComplete {
		status = corev1.ConditionTrue
	}
	condition := hivev1.ClusterInstallCondition{Type: "PreProvisioningCompleted", Status: status, Reason: s.Phase, Message: s.Message, LastTransitionTime: metav1.Now(), LastProbeTime: metav1.Now()}
	found := false
	for i, old := range ici.Status.Conditions {
		if old.Type == condition.Type {
			if old.Status == condition.Status && old.Reason == condition.Reason && old.Message == condition.Message {
				condition.LastTransitionTime = old.LastTransitionTime
			}
			ici.Status.Conditions[i] = condition
			found = true
			break
		}
	}
	if !found {
		ici.Status.Conditions = append(ici.Status.Conditions, condition)
	}
	return ctrl.Result{RequeueAfter: preparationPoll}, r.Status().Update(ctx, ici)
}

func (r *ImageClusterInstallReconciler) ownedPreparationSecret(ctx context.Context, ici *v1alpha1.ImageClusterInstall, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ici.Namespace, Name: name}, secret); err != nil {
		return nil, err
	}
	if !metav1.IsControlledBy(secret, ici) {
		return nil, fmt.Errorf("preparation Secret is owned by another resource")
	}
	return secret, nil
}
func (r *ImageClusterInstallReconciler) createPreparationSecret(ctx context.Context, ici *v1alpha1.ImageClusterInstall, name string, data map[string][]byte) (*corev1.Secret, error) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ici.Namespace}, Immutable: ptr.To(true), Type: corev1.SecretTypeOpaque, Data: data}
	setBackupLabel(secret)
	metav1.SetMetaDataLabel(&secret.ObjectMeta, credentials.SecretResourceLabel, credentials.SecretResourceValue)
	if err := controllerutil.SetControllerReference(ici, secret, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

func (r *ImageClusterInstallReconciler) preparationInputs(ctx context.Context, ici *v1alpha1.ImageClusterInstall, host *bmhv1.BareMetalHost) (*preparationInputs, error) {
	secret, err := r.ownedPreparationSecret(ctx, ici, inputSecretName(ici))
	if apierrors.IsNotFound(err) && ici.Status.PreProvisioning.Attempt == 1 {
		data := &preparationInputs{BaseURL: r.Options.PreparationBaseURL, ImageBaseURL: r.BaseURL}
		data.NMState, err = r.nmstateConfig(ctx, host)
		if err != nil {
			return nil, err
		}
		if ref := ici.Spec.PreProvisioning.ISOServerCACertRef; ref != nil {
			cm := &corev1.ConfigMap{}
			if getErr := r.Get(ctx, client.ObjectKey{Namespace: ici.Namespace, Name: ref.Name}, cm); getErr != nil {
				return nil, getErr
			}
			data.ISOCA = []byte(cm.Data["ca-bundle.crt"])
			if len(data.ISOCA) == 0 {
				return nil, fmt.Errorf("ISO CA ConfigMap must contain ca-bundle.crt")
			}
		}
		if r.Options.PreparationCAFile != "" {
			ca, readErr := os.ReadFile(r.Options.PreparationCAFile)
			if readErr != nil {
				return nil, readErr
			}
			data.CallbackCA = string(ca)
			if !x509.NewCertPool().AppendCertsFromPEM(ca) {
				return nil, fmt.Errorf("preparation callback CA file has no PEM certificates")
			}
		}
		contents, encodeErr := json.Marshal(data)
		if encodeErr != nil {
			return nil, encodeErr
		}
		secret, err = r.createPreparationSecret(ctx, ici, inputSecretName(ici), map[string][]byte{"inputs.json": contents})
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	var data preparationInputs
	if err := json.Unmarshal(secret.Data["inputs.json"], &data); err != nil {
		return nil, err
	}
	return &data, nil
}

func (r *ImageClusterInstallReconciler) ensurePreparationArtifact(ctx context.Context, ici *v1alpha1.ImageClusterInstall, host *bmhv1.BareMetalHost) (*preparation.Payload, error) {
	s := ici.Status.PreProvisioning
	name := preparation.SecretName(string(ici.UID), s.Attempt)
	secret, err := r.ownedPreparationSecret(ctx, ici, name)
	if apierrors.IsNotFound(err) && s.Phase == v1alpha1.PreparationArtifacts {
		inputs, inputErr := r.preparationInputs(ctx, ici, host)
		if inputErr != nil {
			return nil, inputErr
		}
		random := make([]byte, 32)
		if _, randomErr := rand.Read(random); randomErr != nil {
			return nil, randomErr
		}
		callback, callbackErr := url.JoinPath(inputs.BaseURL, "preparation-callbacks", ici.Namespace, ici.Name, string(ici.UID), s.AttemptID)
		if callbackErr != nil {
			return nil, callbackErr
		}
		payload := preparation.Payload{Version: 1, AttemptID: s.AttemptID, HostUID: string(s.HostUID), CallbackURL: callback, Token: base64.RawURLEncoding.EncodeToString(random), CABundle: inputs.CallbackCA, Deadline: s.Deadline.Time, RawNMState: inputs.NMState}
		iso, buildErr := preparation.BuildISO(payload)
		if buildErr != nil {
			return nil, buildErr
		}
		contents, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			return nil, marshalErr
		}
		secret, err = r.createPreparationSecret(ctx, ici, name, map[string][]byte{preparation.PayloadKey: contents, preparation.SecretISOKey: iso})
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	var payload preparation.Payload
	if err := json.Unmarshal(secret.Data[preparation.PayloadKey], &payload); err != nil {
		return nil, err
	}
	if payload.AttemptID != s.AttemptID || payload.HostUID != string(s.HostUID) || payload.Version != 1 || payload.Token == "" {
		return nil, fmt.Errorf("preparation artifact does not match the claimed attempt")
	}
	if len(secret.Data[preparation.SecretISOKey]) == 0 {
		return nil, fmt.Errorf("immutable preparation configuration ISO is missing")
	}
	return &payload, nil
}

func (r *ImageClusterInstallReconciler) startPreparation(ctx context.Context, ici *v1alpha1.ImageClusterInstall, host *bmhv1.BareMetalHost) (ctrl.Result, error) {
	spec := ici.Spec.PreProvisioning
	if err := spec.Validate(); err != nil {
		return ctrl.Result{}, err
	}
	if _, err := preparation.Origin(r.Options.PreparationBaseURL); err != nil {
		return ctrl.Result{}, fmt.Errorf("operator preparation endpoint: %w", err)
	}
	if _, err := preparation.Origin(r.BaseURL); err != nil {
		return ctrl.Result{}, fmt.Errorf("internal image endpoint: %w", err)
	}
	origin, err := preparation.Origin(spec.ISOURL)
	if err != nil {
		return ctrl.Result{}, err
	}
	allowed := false
	for _, entry := range strings.Split(r.Options.PreparationISOOrigins, ",") {
		if candidate, e := preparation.Origin(strings.TrimSpace(entry)); e == nil && candidate == origin {
			allowed = true
		}
	}
	if !allowed {
		return ctrl.Result{}, fmt.Errorf("ISO origin is not in the operator preparation allowlist")
	}
	if !host.DeletionTimestamp.IsZero() || host.Spec.Online || host.Status.PoweredOn || host.Spec.Image != nil || annotationExists(&host.ObjectMeta, ibioManagedBMH) || host.Annotations[preparationOwner] != "" || annotationExists(&host.ObjectMeta, bmhv1.PausedAnnotation) || annotationExists(&host.ObjectMeta, detachedAnnotation) {
		return ctrl.Result{}, fmt.Errorf("preparation requires an unclaimed, offline, attached BareMetalHost without an image")
	}
	if _, err := getDataImage(ctx, r.Client, host.Namespace, host.Name); !apierrors.IsNotFound(err) {
		if err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, fmt.Errorf("an existing DataImage blocks preparation")
	}
	var requests v1alpha1.ImageClusterInstallList
	if err := r.List(ctx, &requests); err != nil {
		return ctrl.Result{}, err
	}
	for _, other := range requests.Items {
		if other.UID != ici.UID && other.Spec.BareMetalHostRef != nil && *other.Spec.BareMetalHostRef == *ici.Spec.BareMetalHostRef && !InstallationCompleted(&other) {
			return ctrl.Result{}, fmt.Errorf("another ImageClusterInstall references the preparation host")
		}
	}
	now := metav1.Now()
	attempt := uuid.NewString()
	boot, _ := url.JoinPath(r.BaseURL, "preparation-images", ici.Namespace, ici.Name, string(ici.UID), attempt, "boot.iso")
	config, _ := url.JoinPath(r.BaseURL, "preparation-images", ici.Namespace, ici.Name, string(ici.UID), attempt, "config.iso")
	ici.Status.PreProvisioning = &v1alpha1.PreProvisioningStatus{Phase: v1alpha1.PreparationArtifacts, AttemptID: attempt, Attempt: 1, HostRef: *ici.Spec.BareMetalHostRef, HostUID: host.UID, BMCAddress: host.Spec.BMC.Address, BootMACAddress: host.Spec.BootMACAddress, StartedAt: now, Deadline: metav1.NewTime(now.Add(spec.AttemptTimeout())), OverallDeadline: metav1.NewTime(now.Add(time.Duration(spec.MaxRetries+1)*spec.AttemptTimeout() + time.Duration(spec.MaxRetries)*spec.Backoff())), BootURL: boot, ConfigURL: config, Message: "Preparing live ISO and per-host configuration"}
	return r.preparationStatus(ctx, ici)
}

func (r *ImageClusterInstallReconciler) reconcilePreparation(ctx context.Context, ici *v1alpha1.ImageClusterInstall, host *bmhv1.BareMetalHost, condition *hivev1.ClusterInstallCondition) (bool, ctrl.Result, error) {
	if ici.Spec.PreProvisioning == nil && ici.Status.PreProvisioning == nil {
		return false, ctrl.Result{}, nil
	}
	condition.Reason = "PreProvisioningInProgress"
	condition.Message = "Preparing server with the IBI live ISO"
	if ici.Status.PreProvisioning == nil {
		result, err := r.startPreparation(ctx, ici, host)
		if err != nil {
			condition.Reason = "PreProvisioningBlocked"
			condition.Message = err.Error()
		}
		return true, result, err
	}
	s := ici.Status.PreProvisioning
	condition.Message = s.Message
	if !preparation.SafeSegment(s.AttemptID) {
		return true, ctrl.Result{}, fmt.Errorf("invalid preparation attempt path")
	}
	if !preparationHostMatches(s, host) || ici.Spec.BareMetalHostRef == nil || *ici.Spec.BareMetalHostRef != s.HostRef {
		return true, ctrl.Result{}, fmt.Errorf("preparation host identity changed")
	}
	if owner := host.Annotations[preparationOwner]; owner != "" && owner != string(ici.UID) {
		return true, ctrl.Result{}, fmt.Errorf("preparation host is claimed by another installation")
	}
	if s.Phase == v1alpha1.PreparationComplete {
		result, err := r.releaseCompletedPreparation(ctx, ici)
		return !result.IsZero() || err != nil, result, err
	}
	if s.Phase == v1alpha1.PreparationFailed {
		condition.Reason = "PreProvisioningFailed"
		return true, ctrl.Result{}, nil
	}
	if !host.DeletionTimestamp.IsZero() {
		return true, ctrl.Result{}, fmt.Errorf("preparation host is being deleted; waiting for media teardown")
	}
	if host.Spec.Image != nil && host.Spec.Image.URL != s.BootURL {
		return true, ctrl.Result{}, fmt.Errorf("preparation host image changed outside the owning workflow")
	}
	receivedResult := s.Phase == v1alpha1.PreparationBooting && s.Result != ""
	expired := !time.Now().Before(s.OverallDeadline.Time) ||
		(s.Phase != v1alpha1.PreparationRetry && !time.Now().Before(s.Deadline.Time))
	if s.Phase != v1alpha1.PreparationStopping && (ici.Spec.PreProvisioning == nil || (!receivedResult && expired)) {
		s.Phase = v1alpha1.PreparationStopping
		s.Result = "failure"
		s.TokenHash = ""
		s.Message = "Preparation timed out; stopping deployment before cleanup"
		result, err := r.preparationStatus(ctx, ici)
		return true, result, err
	}
	var result ctrl.Result
	var err error
	switch s.Phase {
	case v1alpha1.PreparationArtifacts:
		result, err = r.reconcilePreparationArtifacts(ctx, ici, host)
	case v1alpha1.PreparationBooting:
		result, err = r.reconcilePreparationBoot(ctx, ici, host)
	case v1alpha1.PreparationStopping:
		result, err = r.stopPreparation(ctx, ici, host, false)
	case v1alpha1.PreparationRetry:
		result, err = r.reconcilePreparationRetry(ctx, ici, host)
	default:
		return true, ctrl.Result{}, fmt.Errorf("unknown preparation phase %q", s.Phase)
	}
	return true, result, err
}

func (r *ImageClusterInstallReconciler) reconcilePreparationArtifacts(ctx context.Context, ici *v1alpha1.ImageClusterInstall, host *bmhv1.BareMetalHost) (ctrl.Result, error) {
	s := ici.Status.PreProvisioning
	if host.Annotations[preparationOwner] == "" {
		patch := client.MergeFromWithOptions(host.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if host.Annotations == nil {
			host.Annotations = map[string]string{}
		}
		host.Annotations[preparationOwner] = string(ici.UID)
		return ctrl.Result{RequeueAfter: preparationPoll}, r.Patch(ctx, host, patch)
	}
	payload, err := r.ensurePreparationArtifact(ctx, ici, host)
	if err != nil {
		return ctrl.Result{}, err
	}
	inputs, err := r.preparationInputs(ctx, ici, host)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cacheErr := preparation.CacheISO(ctx, ici.Spec.PreProvisioning.ISOURL, ici.Spec.PreProvisioning.ISODigest, strings.Split(r.Options.PreparationISOOrigins, ","), inputs.ISOCA, preparation.BootPath(r.Options.DataDir, ici.Namespace, string(ici.UID), s.AttemptID)); cacheErr != nil {
		return ctrl.Result{}, cacheErr
	}
	if host.Spec.ExternallyProvisioned {
		patch := client.MergeFromWithOptions(host.DeepCopy(), client.MergeFromWithOptimisticLock{})
		host.Spec.ExternallyProvisioned = false
		return ctrl.Result{RequeueAfter: preparationPoll}, r.Patch(ctx, host, patch)
	}
	if host.Status.Provisioning.State != bmhv1.StateAvailable {
		return ctrl.Result{RequeueAfter: preparationPoll}, nil
	}
	image, err := getDataImage(ctx, r.Client, host.Namespace, host.Name)
	if apierrors.IsNotFound(err) {
		image = &bmhv1.DataImage{ObjectMeta: metav1.ObjectMeta{Name: host.Name, Namespace: host.Namespace, Annotations: map[string]string{preparationOwner: string(ici.UID)}}, Spec: bmhv1.DataImageSpec{URL: s.ConfigURL}}
		return ctrl.Result{RequeueAfter: preparationPoll}, r.Create(ctx, image)
	} else if err != nil {
		return ctrl.Result{}, err
	}
	if image.Annotations[preparationOwner] != string(ici.UID) || image.Spec.URL != s.ConfigURL || !image.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, fmt.Errorf("another or deleting DataImage blocks preparation")
	}
	if !controllerutil.ContainsFinalizer(image, bmhv1.DataImageFinalizer) {
		return ctrl.Result{RequeueAfter: preparationPoll}, nil
	}
	s.TokenHash = preparation.TokenHash(payload.Token)
	s.Phase = v1alpha1.PreparationBooting
	s.Message = "Booting live ISO with preparation configuration"
	result, err := r.preparationStatus(ctx, ici)
	return result, err
}

func (r *ImageClusterInstallReconciler) reconcilePreparationBoot(ctx context.Context, ici *v1alpha1.ImageClusterInstall, host *bmhv1.BareMetalHost) (ctrl.Result, error) {
	s := ici.Status.PreProvisioning
	if s.Result != "" || host.Status.ErrorType != "" {
		if s.Result == "" {
			s.Result = "failure"
			s.Message = "BMO reported a preparation deployment failure"
		}
		s.Phase = v1alpha1.PreparationStopping
		s.TokenHash = ""
		if s.Result == "success" {
			s.Message = "Preparation succeeded; stopping live deployment for disk boot"
		} else if host.Status.ErrorType == "" {
			s.Message = "Preparation reported failure; stopping live deployment"
		}
		result, err := r.preparationStatus(ctx, ici)
		return result, err
	}
	// Artifact bytes are persisted in an immutable Secret; restore the
	// large verified boot image after an emptyDir/pod replacement.
	if _, err := r.ensurePreparationArtifact(ctx, ici, host); err != nil {
		return ctrl.Result{}, err
	}
	inputs, err := r.preparationInputs(ctx, ici, host)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cacheErr := preparation.CacheISO(ctx, ici.Spec.PreProvisioning.ISOURL, ici.Spec.PreProvisioning.ISODigest, strings.Split(r.Options.PreparationISOOrigins, ","), inputs.ISOCA, preparation.BootPath(r.Options.DataDir, ici.Namespace, string(ici.UID), s.AttemptID)); cacheErr != nil {
		return ctrl.Result{}, cacheErr
	}
	if host.Spec.Image == nil {
		image, getErr := getDataImage(ctx, r.Client, host.Namespace, host.Name)
		if getErr != nil {
			return ctrl.Result{}, getErr
		}
		if image.Annotations[preparationOwner] != string(ici.UID) || image.Spec.URL != s.ConfigURL || !image.DeletionTimestamp.IsZero() || !controllerutil.ContainsFinalizer(image, bmhv1.DataImageFinalizer) {
			return ctrl.Result{}, fmt.Errorf("required preparation DataImage changed before boot")
		}
		patch := client.MergeFromWithOptions(host.DeepCopy(), client.MergeFromWithOptimisticLock{})
		host.Spec.Image = &bmhv1.Image{URL: s.BootURL, DiskFormat: ptr.To("live-iso")}
		host.Spec.Online = true
		return ctrl.Result{RequeueAfter: preparationPoll}, r.Patch(ctx, host, patch)
	}
	if host.Status.Provisioning.State == bmhv1.StateProvisioned && s.Message != "Waiting for authenticated preparation completion" {
		s.Message = "Waiting for authenticated preparation completion"
		result, err := r.preparationStatus(ctx, ici)
		return result, err
	}
	return ctrl.Result{RequeueAfter: preparationPoll}, nil
}

func (r *ImageClusterInstallReconciler) reconcilePreparationRetry(ctx context.Context, ici *v1alpha1.ImageClusterInstall, host *bmhv1.BareMetalHost) (ctrl.Result, error) {
	s := ici.Status.PreProvisioning
	if s.RetryAt != nil && time.Now().Before(s.RetryAt.Time) {
		return ctrl.Result{RequeueAfter: time.Until(s.RetryAt.Time)}, nil
	}
	s.Attempt++
	s.AttemptID = uuid.NewString()
	s.StartedAt = metav1.Now()
	s.Deadline = metav1.NewTime(s.StartedAt.Add(ici.Spec.PreProvisioning.AttemptTimeout()))
	if s.Deadline.After(s.OverallDeadline.Time) {
		s.Deadline = s.OverallDeadline
	}
	inputs, err := r.preparationInputs(ctx, ici, host)
	if err != nil {
		return ctrl.Result{}, err
	}
	s.BootURL, _ = url.JoinPath(inputs.ImageBaseURL, "preparation-images", ici.Namespace, ici.Name, string(ici.UID), s.AttemptID, "boot.iso")
	s.ConfigURL, _ = url.JoinPath(inputs.ImageBaseURL, "preparation-images", ici.Namespace, ici.Name, string(ici.UID), s.AttemptID, "config.iso")
	s.Phase = v1alpha1.PreparationArtifacts
	s.Result = ""
	s.RetryAt = nil
	s.DeploymentReleased = false
	s.Message = "Preparing a new immutable retry attempt"
	result, err := r.preparationStatus(ctx, ici)
	return result, err
}

func (r *ImageClusterInstallReconciler) stopPreparation(ctx context.Context, ici *v1alpha1.ImageClusterInstall, host *bmhv1.BareMetalHost, deleting bool) (ctrl.Result, error) {
	s := ici.Status.PreProvisioning
	wait := ctrl.Result{RequeueAfter: preparationPoll}
	if !preparationHostMatches(s, host) || host.Annotations[preparationOwner] != string(ici.UID) {
		return wait, fmt.Errorf("preparation cleanup cannot verify host ownership")
	}
	if host.Spec.Image != nil && host.Spec.Image.URL != s.BootURL {
		return wait, fmt.Errorf("preparation cleanup cannot remove another deployment's image")
	}
	if host.Spec.AutomatedCleaningMode != bmhv1.CleaningModeDisabled {
		return wait, fmt.Errorf("preparation cleanup requires automated cleaning to remain disabled")
	}
	if s.DeploymentReleased {
		return r.finishPreparation(ctx, ici, host, deleting)
	}
	if host.Spec.Online {
		patch := client.MergeFromWithOptions(host.DeepCopy(), client.MergeFromWithOptimisticLock{})
		host.Spec.Online = false
		return wait, r.Patch(ctx, host, patch)
	}
	if host.Status.PoweredOn {
		return wait, nil
	}
	if host.Spec.Image != nil || host.Spec.ExternallyProvisioned {
		patch := client.MergeFromWithOptions(host.DeepCopy(), client.MergeFromWithOptimisticLock{})
		host.Spec.Image = nil
		host.Spec.ExternallyProvisioned = false
		return wait, r.Patch(ctx, host, patch)
	}
	if host.Status.Provisioning.State != bmhv1.StateAvailable {
		return wait, nil
	}
	image, err := getDataImage(ctx, r.Client, host.Namespace, host.Name)
	if err == nil {
		if image.Annotations[preparationOwner] != string(ici.UID) || image.Spec.URL != s.ConfigURL {
			return wait, fmt.Errorf("preparation cleanup cannot delete another DataImage")
		}
		if image.DeletionTimestamp.IsZero() {
			return wait, r.Delete(ctx, image, client.Preconditions{UID: ptr.To(image.UID), ResourceVersion: ptr.To(image.ResourceVersion)})
		}
		return wait, nil
	}
	if !apierrors.IsNotFound(err) {
		return wait, err
	}
	// Persist proof of media release before any final handoff patch. A
	// restart after changing the BMH must not start another deprovisioning.
	s.DeploymentReleased = true
	return r.preparationStatus(ctx, ici)
}

func (r *ImageClusterInstallReconciler) releaseCompletedPreparation(ctx context.Context, ici *v1alpha1.ImageClusterInstall) (ctrl.Result, error) {
	s := ici.Status.PreProvisioning
	host, err := getBMH(ctx, r.Client, &s.HostRef)
	if apierrors.IsNotFound(err) && s.DeploymentReleased {
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if host.Annotations[preparationOwner] != string(ici.UID) {
		return ctrl.Result{}, nil
	}
	if !s.DeploymentReleased || !preparationHostMatches(s, host) {
		return ctrl.Result{}, fmt.Errorf("completed preparation cannot verify released host identity")
	}
	patch := client.MergeFromWithOptions(host.DeepCopy(), client.MergeFromWithOptimisticLock{})
	delete(host.Annotations, preparationOwner)
	return ctrl.Result{RequeueAfter: preparationPoll}, r.Patch(ctx, host, patch)
}

func (r *ImageClusterInstallReconciler) finishPreparation(ctx context.Context, ici *v1alpha1.ImageClusterInstall, host *bmhv1.BareMetalHost, deleting bool) (ctrl.Result, error) {
	s := ici.Status.PreProvisioning
	wait := ctrl.Result{RequeueAfter: preparationPoll}
	if !preparation.SafeSegment(ici.Namespace) || !preparation.SafeSegment(string(ici.UID)) || !preparation.SafeSegment(s.AttemptID) {
		return wait, fmt.Errorf("invalid preparation artifact path")
	}
	if secret, err := r.ownedPreparationSecret(ctx, ici, preparation.SecretName(string(ici.UID), s.Attempt)); err == nil {
		return wait, r.Delete(ctx, secret, client.Preconditions{UID: ptr.To(secret.UID), ResourceVersion: ptr.To(secret.ResourceVersion)})
	} else if !apierrors.IsNotFound(err) {
		return wait, err
	}
	directory := filepath.Dir(preparation.BootPath(r.Options.DataDir, ici.Namespace, string(ici.UID), s.AttemptID))
	if err := os.RemoveAll(directory); err != nil {
		return wait, err
	}
	if deleting {
		if secret, err := r.ownedPreparationSecret(ctx, ici, inputSecretName(ici)); err == nil {
			return wait, r.Delete(ctx, secret, client.Preconditions{UID: ptr.To(secret.UID), ResourceVersion: ptr.To(secret.ResourceVersion)})
		} else if !apierrors.IsNotFound(err) {
			return wait, err
		}
		patch := client.MergeFromWithOptions(host.DeepCopy(), client.MergeFromWithOptimisticLock{})
		delete(host.Annotations, preparationOwner)
		return ctrl.Result{}, r.Patch(ctx, host, patch)
	}
	if s.Result == "success" {
		if secret, err := r.ownedPreparationSecret(ctx, ici, inputSecretName(ici)); err == nil {
			return wait, r.Delete(ctx, secret, client.Preconditions{UID: ptr.To(secret.UID), ResourceVersion: ptr.To(secret.ResourceVersion)})
		} else if !apierrors.IsNotFound(err) {
			return wait, err
		}
		patch := client.MergeFromWithOptions(host.DeepCopy(), client.MergeFromWithOptimisticLock{})
		host.Spec.ExternallyProvisioned = true
		// Normal host configuration sets ibioManagedBMH when it requests disk
		// boot. Setting it here would suppress that power-on request.
		if err := r.Patch(ctx, host, patch); err != nil {
			return wait, err
		}
		s.Phase = v1alpha1.PreparationComplete
		s.Message = "Server prepared; continuing normal IBI cluster installation"
	} else if ici.Spec.PreProvisioning != nil && s.Attempt <= ici.Spec.PreProvisioning.MaxRetries && time.Now().Add(ici.Spec.PreProvisioning.Backoff()).Before(s.OverallDeadline.Time) {
		s.Phase = v1alpha1.PreparationRetry
		s.RetryAt = ptr.To(metav1.NewTime(time.Now().Add(ici.Spec.PreProvisioning.Backoff())))
		s.Message = "Previous deployment released; waiting before retry"
	} else {
		s.Phase = v1alpha1.PreparationFailed
		s.Message += "; deployment and media cleanup completed"
	}
	return r.preparationStatus(ctx, ici)
}

func (r *ImageClusterInstallReconciler) finalizePreparation(ctx context.Context, ici *v1alpha1.ImageClusterInstall) (bool, ctrl.Result, error) {
	s := ici.Status.PreProvisioning
	host, err := getBMH(ctx, r.Client, &s.HostRef)
	if err != nil {
		if apierrors.IsNotFound(err) && s.DeploymentReleased {
			return false, ctrl.Result{}, nil
		}
		return true, ctrl.Result{}, err
	}
	if !preparationHostMatches(s, host) {
		if s.DeploymentReleased {
			return false, ctrl.Result{}, nil
		}
		return true, ctrl.Result{}, fmt.Errorf("preparation cleanup host identity changed")
	}
	if s.DeploymentReleased && host.Annotations[preparationOwner] == "" {
		return false, ctrl.Result{}, nil
	}
	if s.Phase != v1alpha1.PreparationStopping || s.TokenHash != "" {
		s.Phase = v1alpha1.PreparationStopping
		s.TokenHash = ""
		s.Message = "Cancelling preparation and waiting for deployment teardown"
		result, updateErr := r.preparationStatus(ctx, ici)
		return true, result, updateErr
	}
	if host.Annotations[preparationOwner] == "" && host.Spec.Image == nil && !host.Spec.Online && !host.Status.PoweredOn {
		// The attempt can be deleted before the host was ever claimed.
		s.DeploymentReleased = true
		result, updateErr := r.preparationStatus(ctx, ici)
		return true, result, updateErr
	}
	result, err := r.stopPreparation(ctx, ici, host, true)
	if err == nil && result.IsZero() {
		s.DeploymentReleased = true
		result, err = r.preparationStatus(ctx, ici)
	}
	return true, result, err
}
