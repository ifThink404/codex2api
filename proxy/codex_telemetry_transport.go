package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

func sendCodexTelemetryJob(job codexTelemetryJob) error {
	ctx, cancel := context.WithTimeout(context.Background(), codexTelemetryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, job.url, bytes.NewReader(job.body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	if job.metrics {
		req.Header.Set("User-Agent", "OTel-OTLP-Exporter-Rust/0.31.0")
		req.Header.Set("statsig-api-key", codexStatsigAPIKey())
	} else {
		applyCodexAnalyticsHeaders(req.Header, job.client)
	}
	resp, err := getPooledClient(job.client.account, job.client.proxyURL).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &codexTelemetryHTTPError{status: resp.StatusCode}
	}
	return nil
}

type codexTelemetryHTTPError struct{ status int }

func (e *codexTelemetryHTTPError) Error() string { return http.StatusText(e.status) }

func applyCodexAnalyticsHeaders(headers http.Header, client codexTelemetryClient) {
	headers.Set("Authorization", "Bearer "+client.accessToken)
	headers.Set("Chatgpt-Account-Id", client.accountID)
	headers.Set("User-Agent", client.userAgent)
	headers.Set("Originator", client.originator)
}
