package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBPSHTTPPhasesStandardAndReuse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(15 * time.Millisecond)
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()
	client := server.Client()
	for round := 0; round < 2; round++ {
		ctx := context.WithValue(t.Context(), bpsTimingContextKey{}, &bpsTimingDiagnostic{})
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
		require.NoError(t, err)
		req, trace := traceBPSHTTP(req)
		resp, err := client.Do(req)
		require.NoError(t, err)
		phases := trace.finish(err)
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.NotNil(t, phases.ConnectionAcquireMS)
		require.NotNil(t, phases.ConnectionReused)
		require.Equal(t, round > 0, *phases.ConnectionReused)
		require.NotNil(t, phases.WroteRequestMS)
		require.NotNil(t, phases.FirstResponseByteMS)
		require.NotNil(t, phases.ResponseWaitMS)
		require.GreaterOrEqual(t, *phases.ResponseWaitMS, int64(10))
		require.Nil(t, phases.TLSMS)
		raw, err := json.Marshal(phases)
		require.NoError(t, err)
		require.NotContains(t, string(raw), server.URL)
		var decoded bpsHTTPPhases
		require.NoError(t, json.Unmarshal(raw, &decoded))
		require.Equal(t, *phases, decoded)
	}
}

func TestBPSHTTPPhasesUTLSHandshakeFailure(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected authenticated HTTP request") }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, bpsTimingContextKey{}, &bpsTimingDiagnostic{})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	req, trace := traceBPSHTTP(req)
	_, err = NewUTLSHttpClient("").Do(req)
	require.Error(t, err) // The local certificate is deliberately untrusted.
	phases := trace.finish(err)
	require.Equal(t, "utls", phases.Source)
	require.True(t, phases.Failed)
	require.NotNil(t, phases.DialMS)
	require.NotNil(t, phases.TLSMS)
	require.Nil(t, phases.FirstResponseByteMS)
	require.Nil(t, phases.ResponseWaitMS)
}
