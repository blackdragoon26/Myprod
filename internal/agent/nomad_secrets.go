package agent

// This client deliberately never includes response bodies or request values in
// errors: Nomad variable responses contain decrypted application credentials.
import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

var errNomadMissing = errors.New("Nomad record not found")

type nomadRequest func(context.Context, string, string, any, any) error

func newNomadSecretClient() (nomadRequest, error) {
	ca, err := os.ReadFile("/etc/nomad.d/tls/nomad-agent-ca.pem")
	if err != nil {
		return nil, errors.New("cannot load Nomad CA for credential storage")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid Nomad CA")
	}
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return func(ctx context.Context, method, path string, body, result any) error {
		var raw []byte
		if body != nil {
			var err error
			raw, err = json.Marshal(body)
			if err != nil {
				return errors.New("cannot encode Nomad request")
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, "https://10.44.0.1:4646"+path, bytes.NewReader(raw))
		if err != nil {
			return errors.New("cannot create Nomad request")
		}
		token := firstExistingToken()
		if token == "" {
			return errors.New("Nomad ACL credential unavailable")
		}
		req.Header.Set("X-Nomad-Token", token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return errors.New("Nomad credential storage unavailable")
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return errNomadMissing
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("Nomad request failed (HTTP %d); no credential values returned", resp.StatusCode)
		}
		if result != nil {
			if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(result); err != nil {
				return errors.New("invalid Nomad response")
			}
		}
		return nil
	}, nil
}
