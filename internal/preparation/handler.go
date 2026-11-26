// Copyright 2026 Red Hat, Inc.
// SPDX-License-Identifier: Apache-2.0

package preparation

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openshift/image-based-install-operator/api/v1alpha1"
)

type Handler struct {
	Client  client.Client
	DataDir string
}

var errAttemptMissing = errors.New("preparation attempt not found")

func lookupStatus(err error) int {
	if apierrors.IsNotFound(err) || errors.Is(err, errAttemptMissing) {
		return http.StatusNotFound
	}
	return http.StatusServiceUnavailable
}

func TokenHash(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

// CallbackMux exposes only authenticated callbacks to preparation nodes.
func (h *Handler) CallbackMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /preparation-callbacks/{namespace}/{name}/{uid}/{attempt}", h.callback)
	return mux
}

// ImageMux stays on the internal image listener. Ironic publishes its own
// BMC-facing copies, so the callback endpoint need not expose these images.
func (h *Handler) ImageMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /preparation-images/{namespace}/{name}/{uid}/{attempt}/{image}", h.image)
	return mux
}

func (h *Handler) current(r *http.Request) (*v1alpha1.ImageClusterInstall, error) {
	ici := &v1alpha1.ImageClusterInstall{}
	key := types.NamespacedName{Namespace: r.PathValue("namespace"), Name: r.PathValue("name")}
	if err := h.Client.Get(r.Context(), key, ici); err != nil {
		return nil, err
	}
	status := ici.Status.PreProvisioning
	if status == nil || string(ici.UID) != r.PathValue("uid") || status.AttemptID != r.PathValue("attempt") ||
		!SafeSegment(ici.Namespace) || !SafeSegment(string(ici.UID)) || !SafeSegment(status.AttemptID) {
		return nil, errAttemptMissing
	}
	return ici, nil
}

func (h *Handler) image(w http.ResponseWriter, r *http.Request) {
	ici, err := h.current(r)
	if err != nil {
		http.Error(w, "preparation image unavailable", lookupStatus(err))
		return
	}
	if ici.Status.PreProvisioning.DeploymentReleased {
		http.NotFound(w, r)
		return
	}
	status := ici.Status.PreProvisioning
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/x-iso9660-image")
	switch r.PathValue("image") {
	case "config.iso":
		secret := &corev1.Secret{}
		if err := h.Client.Get(r.Context(), client.ObjectKey{Namespace: ici.Namespace, Name: SecretName(string(ici.UID), status.Attempt)}, secret); err != nil {
			http.Error(w, "preparation image unavailable", lookupStatus(err))
			return
		}
		if !ownedBy(secret, ici.UID) {
			http.NotFound(w, r)
			return
		}
		data, ok := secret.Data[SecretISOKey]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "config.iso", time.Time{}, bytes.NewReader(data))
	case "boot.iso":
		path := BootPath(h.DataDir, ici.Namespace, string(ici.UID), status.AttemptID)
		marker, err := os.ReadFile(path + ".sha256")
		if err != nil || ici.Spec.PreProvisioning == nil || string(marker) != ici.Spec.PreProvisioning.ISODigest {
			http.Error(w, "verified boot image is being restored", http.StatusServiceUnavailable)
			return
		}
		http.ServeFile(w, r, path)
	default:
		http.NotFound(w, r)
	}
}

func ownedBy(object metav1.Object, uid types.UID) bool {
	for _, owner := range object.GetOwnerReferences() {
		if owner.UID == uid {
			return true
		}
	}
	return false
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	body := struct {
		Result  string    `json:"result"`
		HostUID types.UID `json:"hostUID"`
	}{}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || (body.Result != "success" && body.Result != "failure") {
		http.Error(w, "invalid callback", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "invalid callback", http.StatusBadRequest)
		return
	}
	code := http.StatusOK
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ici, err := h.current(r)
		if err != nil {
			code = lookupStatus(err)
			return nil
		}
		status := ici.Status.PreProvisioning
		provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || status.TokenHash == "" ||
			subtle.ConstantTimeCompare([]byte(TokenHash(provided)), []byte(status.TokenHash)) != 1 {
			code = http.StatusUnauthorized
			return nil
		}
		if !ici.DeletionTimestamp.IsZero() || status.Phase != v1alpha1.PreparationBooting ||
			!time.Now().Before(status.Deadline.Time) || body.HostUID != status.HostUID {
			code = http.StatusConflict
			return nil
		}
		if status.Result != "" {
			if status.Result != body.Result {
				code = http.StatusConflict
			}
			return nil
		}
		status.Result = body.Result
		return h.Client.Status().Update(r.Context(), ici)
	})
	if err != nil {
		http.Error(w, "could not persist callback; retry", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(code)
}
