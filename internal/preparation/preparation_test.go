// Copyright 2026 Red Hat, Inc.
// SPDX-License-Identifier: Apache-2.0

package preparation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/openshift/image-based-install-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestPreparationISOLayout(t *testing.T) {
	input := Payload{Version: 1, AttemptID: "attempt", HostUID: "host-uid", Token: "private-attempt-token", CallbackURL: "https://hub.example.com/callback", RawNMState: "interfaces: []\n", Deadline: time.Now().Add(time.Hour)}
	content, err := BuildISO(input)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.iso")
	if err = os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	image, err := diskfs.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer image.File.Close()
	fs, err := image.GetFilesystem(0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimRight(fs.Label(), "\x00 ") != "cluster-config" {
		t.Fatalf("unexpected label %q", fs.Label())
	}
	read := func(path string) []byte {
		f, err := fs.OpenFile(path, os.O_RDONLY)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		data, err := io.ReadAll(f)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	var manifest struct {
		Version int    `json:"api_version"`
		Network string `json:"raw_nm_state_config"`
	}
	if err := json.Unmarshal(read("/cluster-configuration/manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version != 1 || manifest.Network != input.RawNMState {
		t.Fatal("network manifest was not preserved")
	}
	var payload Payload
	if err := json.Unmarshal(read("/ibi-preparation/config.json"), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Token != input.Token || payload.AttemptID != input.AttemptID || payload.HostUID != input.HostUID {
		t.Fatal("attempt payload was not preserved")
	}
}

func TestCacheISOVerifiesAndRetainsExactBytes(t *testing.T) {
	content := []byte("immutable diagnostic ISO bytes")
	count := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { count++; _, _ = w.Write(content) }))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	sum := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	dest := filepath.Join(t.TempDir(), "boot.iso")
	if err := CacheISO(t.Context(), server.URL, digest, []string{"https://other.example.com"}, ca, dest); err == nil {
		t.Fatal("accepted unapproved origin")
	}
	if count != 0 {
		t.Fatal("fetched an unapproved URL")
	}
	if err := CacheISO(t.Context(), server.URL, "sha256:"+strings.Repeat("0", 64), []string{server.URL}, ca, dest); err == nil {
		t.Fatal("accepted wrong checksum")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("published an unverified artifact")
	}
	if err := CacheISO(t.Context(), server.URL, digest, []string{server.URL}, ca, dest); err != nil {
		t.Fatal(err)
	}
	before := count
	content = []byte("changed source")
	if err := CacheISO(t.Context(), server.URL, digest, []string{server.URL}, ca, dest); err != nil {
		t.Fatal(err)
	}
	if count != before {
		t.Fatal("replaced a verified immutable cache entry")
	}
	stored, err := os.ReadFile(dest)
	if err != nil || string(stored) != "immutable diagnostic ISO bytes" {
		t.Fatal("cache contents changed")
	}
	// EmptyDir replacement must fetch and verify again, never trust the URL.
	if err := os.RemoveAll(filepath.Dir(dest)); err != nil {
		t.Fatal(err)
	}
	if err := CacheISO(t.Context(), server.URL, digest, []string{server.URL}, ca, dest); err == nil {
		t.Fatal("accepted changed bytes after cache loss")
	}
}

func TestCacheISORejectsRedirect(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://other.example.com/image.iso", http.StatusFound)
	}))
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := CacheISO(t.Context(), server.URL, "sha256:"+strings.Repeat("0", 64), []string{server.URL}, ca, filepath.Join(t.TempDir(), "boot.iso")); err == nil {
		t.Fatal("followed a redirect")
	}
}

func newCallbackTest(t *testing.T) (client.Client, *v1alpha1.ImageClusterInstall, http.Handler) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ici := &v1alpha1.ImageClusterInstall{ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "target", UID: "ici-uid"}, Status: v1alpha1.ImageClusterInstallStatus{PreProvisioning: &v1alpha1.PreProvisioningStatus{Phase: v1alpha1.PreparationBooting, AttemptID: "attempt", HostUID: "host-uid", TokenHash: TokenHash("secret-token"), Deadline: metav1.NewTime(time.Now().Add(time.Hour))}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(ici).WithObjects(ici).Build()
	handler := (&Handler{Client: c, DataDir: t.TempDir()}).CallbackMux()
	return c, ici, handler
}

func TestCallbacksAreBoundToAttemptAndResultIsImmutable(t *testing.T) {
	c, ici, handler := newCallbackTest(t)
	send := func(path, token, body string) int {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	path := "/preparation-callbacks/target/cluster/ici-uid/attempt"
	body := `{"result":"success","hostUID":"host-uid"}`
	for _, tc := range []struct {
		name, path, token, body string
		want                    int
	}{
		{"wrong token", path, "wrong", body, 401},
		{"old attempt", "/preparation-callbacks/target/cluster/ici-uid/old", "secret-token", body, 404},
		{"recreated ICI", "/preparation-callbacks/target/cluster/new-uid/attempt", "secret-token", body, 404},
		{"other host", path, "secret-token", `{"result":"success","hostUID":"other"}`, 409},
		{"trailing input", path, "secret-token", body + `{}`, 400},
		{"success", path, "secret-token", body, 200},
		{"idempotent retry", path, "secret-token", body, 200},
		{"conflicting failure", path, "secret-token", `{"result":"failure","hostUID":"host-uid"}`, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := send(tc.path, tc.token, tc.body); got != tc.want {
				t.Fatalf("HTTP %d, want %d", got, tc.want)
			}
		})
	}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(ici), ici); err != nil {
		t.Fatal(err)
	}
	if ici.Status.PreProvisioning.Result != "success" {
		t.Fatal("result was not persisted")
	}
	data, _ := json.Marshal(ici.Status)
	if bytes.Contains(data, []byte("secret-token")) {
		t.Fatal("callback token leaked into status")
	}
	ici.Status.PreProvisioning.Deadline = metav1.NewTime(time.Now().Add(-time.Second))
	if err := c.Status().Update(t.Context(), ici); err != nil {
		t.Fatal(err)
	}
	if got := send(path, "secret-token", body); got != 409 {
		t.Fatalf("expired callback: HTTP %d", got)
	}
	r := httptest.NewRequest(http.MethodGet, "/images/target/private.iso", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal("preparation listener exposes normal configuration images")
	}
}

func TestConfigurationISOHasImmutableOwnerAndSupportsRangeFetch(t *testing.T) {
	c, ici, _ := newCallbackTest(t)
	handler := (&Handler{Client: c, DataDir: t.TempDir()}).ImageMux()
	ici.Status.PreProvisioning.Attempt = 1
	if err := c.Status().Update(t.Context(), ici); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: SecretName(string(ici.UID), 1), Namespace: ici.Namespace, OwnerReferences: []metav1.OwnerReference{{APIVersion: v1alpha1.GroupVersion.String(), Kind: "ImageClusterInstall", Name: ici.Name, UID: ici.UID, Controller: ptr.To(true)}}}, Data: map[string][]byte{SecretISOKey: []byte("configuration-iso")}}
	if err := c.Create(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	path := "/preparation-images/target/cluster/ici-uid/attempt/config.iso"
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Range", "bytes=0-3")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 206 || w.Body.String() != "conf" {
		t.Fatalf("unexpected range response: %d %q", w.Code, w.Body.String())
	}
	secret.OwnerReferences[0].UID = types.UID("other")
	if err := c.Update(t.Context(), secret); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != 404 {
		t.Fatal("served another owner's Secret")
	}
}

func TestIgnitionRequiresConfigurationBeforePreparationAndPreservesSSH(t *testing.T) {
	source := []byte(`{"ignition":{"version":"3.2.0"},"passwd":{"users":[{"name":"core","sshAuthorizedKeys":["existing-key"]}]},"systemd":{"units":[{"name":"install-rhcos-and-restore-seed.service","contents":"[Service]\nType=oneshot\nExecStart=/prepare"}]}}`)
	output, err := AddIgnition(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"existing-key", "ExecStart=/prepare", "Requires=ibi-preparation-bootstrap.service", "ExecStartPost=/usr/local/libexec/ibi-preparation-agent report success", "OnFailure=ibi-preparation-failed.service"} {
		if !bytes.Contains(output, []byte(expected)) {
			t.Fatalf("missing %q", expected)
		}
	}
	if _, err := AddIgnition(output); err == nil {
		t.Fatal("accepted duplicate preparation units")
	}
	if _, err := AddIgnition([]byte(`{"ignition":{"version":"3.2.0"}}`)); err == nil {
		t.Fatal("accepted missing disk preparation service")
	}
}

func TestGuestCallbackDoesNotExposeTokenInArguments(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("requires jq")
	}
	dir := t.TempDir()
	script, err := Assets.ReadFile("assets/ibi-preparation-agent")
	if err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(dir, "agent")
	if err = os.WriteFile(agent, script, 0700); err != nil {
		t.Fatal(err)
	}
	fakeCurl := []byte("#!/bin/bash\nprintf '%s\\n' \"$@\" > \"$IBI_PREPARATION_RUNTIME_DIR/curl-args\"\nprintf '200'\n")
	if err = os.WriteFile(filepath.Join(dir, "curl"), fakeCurl, 0700); err != nil {
		t.Fatal(err)
	}
	payload := Payload{Version: 1, HostUID: "host-uid", Token: "private-test-token", CallbackURL: "https://hub.example.com/callback", Deadline: time.Now().Add(time.Hour)}
	data, _ := json.Marshal(payload)
	if err = os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", agent, "report", "success")
	cmd.Env = append(os.Environ(), "IBI_PREPARATION_RUNTIME_DIR="+dir, "PATH="+dir+":"+os.Getenv("PATH"))
	if output, runErr := cmd.CombinedOutput(); runErr != nil {
		t.Fatalf("%v: %s", runErr, output)
	}
	args, err := os.ReadFile(filepath.Join(dir, "curl-args"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(args, []byte(payload.Token)) || bytes.Contains(args, []byte("--insecure")) {
		t.Fatalf("unsafe curl arguments: %s", args)
	}
	contents, err := os.ReadFile(filepath.Join(dir, "authorization.header"))
	if err != nil || strings.TrimSpace(string(contents)) != "Authorization: Bearer "+payload.Token {
		t.Fatal("callback authorization missing")
	}
	result, err := os.ReadFile(filepath.Join(dir, "result.json"))
	var reported struct {
		HostUID string `json:"hostUID"`
	}
	if err != nil || json.Unmarshal(result, &reported) != nil || reported.HostUID != "host-uid" {
		t.Fatal("callback host identity missing")
	}
}

func TestGuestReportsPreparationOverVerifiedTLS(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("requires curl")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("requires jq")
	}
	c, ici, handler := newCallbackTest(t)
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	dir := t.TempDir()
	script, err := Assets.ReadFile("assets/ibi-preparation-agent")
	if err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(dir, "agent")
	if err = os.WriteFile(agent, script, 0700); err != nil {
		t.Fatal(err)
	}
	payload := Payload{Version: 1, HostUID: "host-uid", Token: "secret-token", CallbackURL: server.URL + "/preparation-callbacks/target/cluster/ici-uid/attempt", CABundle: string(ca), Deadline: time.Now().Add(5 * time.Second)}
	contents, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "config.json"), contents, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "bash", agent, "report", "success")
	command.Env = append(os.Environ(), "IBI_PREPARATION_RUNTIME_DIR="+dir)
	if output, runErr := command.CombinedOutput(); runErr != nil {
		t.Fatalf("guest callback failed: %v: %s", runErr, output)
	}
	if err = c.Get(t.Context(), client.ObjectKeyFromObject(ici), ici); err != nil {
		t.Fatal(err)
	}
	if ici.Status.PreProvisioning.Result != "success" {
		t.Fatal("guest callback was not durably recorded")
	}
}

func TestIgnitionRejectsPrematureCompletionServiceTypes(t *testing.T) {
	for _, test := range []struct {
		name, contents, override string
		valid                    bool
	}{
		{name: "oneshot", contents: "[Service]\nType=oneshot\nExecStart=/prepare", valid: true},
		{name: "implicit simple", contents: "[Service]\nExecStart=/prepare"},
		{name: "simple", contents: "[Service]\nType=simple\nExecStart=/prepare"},
		{name: "later drop-in changes type", contents: "[Service]\nType=oneshot\nExecStart=/prepare", override: "[Service]\nType=exec"},
		{name: "drop-in sets oneshot", contents: "[Service]\nExecStart=/prepare", override: "[Service]\nType=oneshot", valid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := map[string]any{"name": "install-rhcos-and-restore-seed.service", "contents": test.contents}
			if test.override != "" {
				service["dropins"] = []any{map[string]any{"name": "99-custom.conf", "contents": test.override}}
			}
			input, err := json.Marshal(map[string]any{"ignition": map[string]any{"version": "3.2.0"}, "systemd": map[string]any{"units": []any{service}}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = AddIgnition(input)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, got %v", test.valid, err)
			}
		})
	}
}
