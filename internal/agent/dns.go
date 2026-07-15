package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const netlifyAPIBase = "https://api.netlify.com/api/v1"

type dnsCapability struct {
	Provider   string `json:"provider"`
	Zone       string `json:"zone"`
	TargetIPv4 string `json:"targetIPv4"`
	Configured bool   `json:"configured"`
}

type dnsResult struct {
	Status  string
	Message string
}

type dnsManager interface {
	Capability() dnsCapability
	EnsureA(context.Context, string) (dnsResult, error)
}

type netlifyDNS struct {
	token      string
	zone       string
	targetIPv4 string
	client     *http.Client
	lookupHost func(context.Context, string) ([]string, error)
	apiBase    string
}

type netlifyZone struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Domain string `json:"domain"`
}

type netlifyRecord struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	Type     string `json:"type"`
	Value    string `json:"value"`
}

func newNetlifyDNSFromEnv() *netlifyDNS {
	return &netlifyDNS{
		token:      strings.TrimSpace(os.Getenv("NETLIFY_AUTH_TOKEN")),
		zone:       normalizeHostname(os.Getenv("MYPROD_DNS_ZONE")),
		targetIPv4: strings.TrimSpace(os.Getenv("MYPROD_INGRESS_IPV4")),
		client:     &http.Client{Timeout: 12 * time.Second},
		lookupHost: net.DefaultResolver.LookupHost,
		apiBase:    netlifyAPIBase,
	}
}

func (d *netlifyDNS) Capability() dnsCapability {
	if d == nil {
		return dnsCapability{Provider: "netlify"}
	}
	parsedTarget := net.ParseIP(d.targetIPv4)
	configured := d.token != "" && d.zone != "" && parsedTarget != nil && parsedTarget.To4() != nil
	return dnsCapability{
		Provider:   "netlify",
		Zone:       d.zone,
		TargetIPv4: d.targetIPv4,
		Configured: configured,
	}
}

func (d *netlifyDNS) EnsureA(ctx context.Context, hostname string) (dnsResult, error) {
	capability := d.Capability()
	if !capability.Configured {
		return dnsResult{Status: "unconfigured", Message: "Netlify DNS credentials are not configured on Oracle"}, errors.New("Netlify DNS automation is not configured on Oracle")
	}
	hostname = normalizeHostname(hostname)
	if hostname == d.zone || !strings.HasSuffix(hostname, "."+d.zone) {
		return dnsResult{Status: "error", Message: "hostname is outside the configured DNS zone"}, fmt.Errorf("hostname %q is outside managed zone %q", hostname, d.zone)
	}

	zoneID, err := d.findZone(ctx)
	if err != nil {
		return dnsResult{Status: "error", Message: "Netlify zone lookup failed"}, err
	}
	records, err := d.listRecords(ctx, zoneID)
	if err != nil {
		return dnsResult{Status: "error", Message: "Netlify record lookup failed"}, err
	}
	foundTarget := false
	for _, record := range records {
		if normalizeHostname(record.Hostname) != hostname {
			continue
		}
		recordType := strings.ToUpper(record.Type)
		if recordType == "A" && strings.TrimSpace(record.Value) == d.targetIPv4 {
			foundTarget = true
			continue
		}
		if recordType == "A" || recordType == "AAAA" || recordType == "CNAME" {
			message := fmt.Sprintf("conflicting %s record already points to %s", recordType, record.Value)
			return dnsResult{Status: "conflict", Message: message}, errors.New(message)
		}
	}

	if !foundTarget {
		payload := map[string]any{
			"type":     "A",
			"hostname": hostname,
			"value":    d.targetIPv4,
			"ttl":      300,
		}
		var created netlifyRecord
		if err := d.request(ctx, http.MethodPost, "/dns_zones/"+url.PathEscape(zoneID)+"/dns_records", payload, &created); err != nil {
			return dnsResult{Status: "error", Message: "Netlify record creation failed"}, err
		}
	}

	if d.waitForResolution(ctx, hostname) {
		return dnsResult{Status: "ready", Message: "A record resolves to " + d.targetIPv4}, nil
	}
	return dnsResult{Status: "pending", Message: "A record exists at Netlify; public DNS propagation is pending"}, nil
}

func (d *netlifyDNS) findZone(ctx context.Context) (string, error) {
	var zones []netlifyZone
	if err := d.request(ctx, http.MethodGet, "/dns_zones?per_page=100", nil, &zones); err != nil {
		return "", err
	}
	for _, zone := range zones {
		if normalizeHostname(zone.Name) == d.zone || normalizeHostname(zone.Domain) == d.zone {
			return zone.ID, nil
		}
	}
	return "", fmt.Errorf("Netlify DNS zone %q was not found", d.zone)
}

func (d *netlifyDNS) listRecords(ctx context.Context, zoneID string) ([]netlifyRecord, error) {
	var records []netlifyRecord
	err := d.request(ctx, http.MethodGet, "/dns_zones/"+url.PathEscape(zoneID)+"/dns_records?per_page=100", nil, &records)
	return records, err
}

func (d *netlifyDNS) waitForResolution(ctx context.Context, hostname string) bool {
	for attempt := 0; attempt < 6; attempt++ {
		addresses, err := d.lookupHost(ctx, hostname)
		if err == nil {
			for _, address := range addresses {
				if address == d.targetIPv4 {
					return true
				}
			}
		}
		if attempt == 5 {
			break
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
	return false
}

func (d *netlifyDNS) request(ctx context.Context, method, path string, payload any, output any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(d.apiBase, "/")+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("Netlify API request failed: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiError struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		_ = json.Unmarshal(data, &apiError)
		message := strings.TrimSpace(apiError.Message)
		if message == "" {
			message = strings.TrimSpace(apiError.Error)
		}
		if message == "" {
			message = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("Netlify API returned %d: %s", resp.StatusCode, message)
	}
	if output != nil && len(data) > 0 {
		if err := json.Unmarshal(data, output); err != nil {
			return fmt.Errorf("decode Netlify API response: %w", err)
		}
	}
	return nil
}

func normalizeHostname(value string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
}
