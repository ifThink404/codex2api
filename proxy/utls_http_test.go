package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
)

func TestUTLSResponsesAPIAllowsPlainHTTP(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "utls_chrome")
	for _, throughProxy := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "http_proxy"}[throughProxy], func(t *testing.T) {
			seen := make(chan *http.Request, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- r.Clone(r.Context())
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp_http","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer upstream.Close()
			base, proxyURL := upstream.URL, ""
			if throughProxy {
				// This host cannot resolve: a successful request must use the proxy.
				base, proxyURL = "http://relay.invalid", upstream.URL
			}
			account := &auth.Account{DBID: 48127, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: base, APIKey: "local-api-key", ProxyURL: proxyURL}
			defer recyclePooledClient(account, proxyURL)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			for _, compact := range []bool{false, true} {
				var response *http.Response
				var err error
				body := []byte(`{"model":"gpt-5.6-sol","input":"hello","stream":false}`)
				if compact {
					response, err = ExecuteOpenAIResponsesCompactRequest(ctx, account, body, "", nil)
				} else {
					response, err = ExecuteOpenAIResponsesRequest(ctx, account, body, "", nil)
				}
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, response.StatusCode)
				_, err = io.ReadAll(response.Body)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				request := <-seen
				require.Nil(t, request.TLS)
				require.Equal(t, "Bearer local-api-key", request.Header.Get("Authorization"))
				path := "/v1/responses"
				if compact {
					path += "/compact"
				}
				require.Equal(t, path, request.URL.Path)
				if throughProxy {
					require.Equal(t, base+path, request.RequestURI)
				}
			}
		})
	}
}

func TestUTLSHTTPSDoesNotFallbackToPlainHTTP(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("untrusted TLS server must not receive an HTTP request")
	}))
	defer server.Close()
	transport := NewUTLSTransport("").(*utlsRoundTripper)
	defer transport.CloseAllConnections()
	transport.plainHTTP = diagnosticRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Error("HTTPS must not use the plaintext transport")
		return nil, io.EOF
	})
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	_, err = transport.RoundTrip(request)
	require.Error(t, err)
	require.Contains(t, err.Error(), "TLS")
}

func TestUTLSPlainHTTPHonorsCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	transport := NewUTLSTransport("").(*utlsRoundTripper)
	defer transport.CloseAllConnections()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader("body"))
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { _, err := transport.RoundTrip(request); done <- err }()
	<-started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}
