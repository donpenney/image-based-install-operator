// Copyright 2026 Red Hat, Inc.
// SPDX-License-Identifier: Apache-2.0

package preparation

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func Origin(value string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", fmt.Errorf("an absolute HTTPS URL without credentials or fragment is required")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return "https://" + net.JoinHostPort(strings.TrimSuffix(strings.ToLower(u.Hostname()), "."), port), nil
}

// CacheISO verifies the same bytes that the preparation server serves to
// Ironic. It never publishes an incomplete or unverified download.
func CacheISO(ctx context.Context, source, digest string, allowed []string, ca []byte, destination string) error {
	origin, err := Origin(source)
	if err != nil {
		return err
	}
	approved := false
	for _, value := range allowed {
		value = strings.TrimSpace(value)
		if candidate, originErr := Origin(value); originErr == nil && candidate == origin {
			approved = true
		}
	}
	if !approved {
		return fmt.Errorf("ISO origin is not in the operator allowlist")
	}
	expected := strings.TrimPrefix(digest, "sha256:")
	if decoded, decodeErr := hex.DecodeString(expected); decodeErr != nil || len(decoded) != sha256.Size || !strings.HasPrefix(digest, "sha256:") {
		return fmt.Errorf("a SHA-256 ISO digest is required")
	}
	if marker, markerErr := os.ReadFile(destination + ".sha256"); markerErr == nil && string(marker) == digest {
		if info, statErr := os.Lstat(destination); statErr == nil && info.Mode().IsRegular() {
			return nil
		}
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		return err
	}
	if len(ca) != 0 && !roots.AppendCertsFromPEM(ca) {
		return fmt.Errorf("ISO CA bundle has no PEM certificates")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Minute,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return fmt.Errorf("ISO redirects are not supported; use the final allowlisted URL")
		},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("ISO server returned HTTP %d", response.StatusCode)
	}
	if err = os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(destination), ".download-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	hash := sha256.New()
	const maximum = int64(20 * 1024 * 1024 * 1024)
	count, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, maximum+1))
	if err != nil {
		return err
	}
	if count > maximum {
		return fmt.Errorf("live ISO exceeds the 20 GiB size limit")
	}
	if hex.EncodeToString(hash.Sum(nil)) != expected {
		return fmt.Errorf("live ISO SHA-256 digest does not match")
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), destination); err != nil {
		return err
	}
	return os.WriteFile(destination+".sha256", []byte(digest), 0600)
}
