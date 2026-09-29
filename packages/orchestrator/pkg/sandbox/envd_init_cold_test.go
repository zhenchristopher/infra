//go:build linux

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A cold /init must finish its context-bound setup before subsequent requests can
// use the warm path. Canceling every attempt at the clock-freshness threshold
// restarts that work forever. A slow success still needs a fresh timestamp pass.
func TestEnvdInitColdSetupAndFreshTimestamp(t *testing.T) { //nolint:paralleltest // overrides sandboxHttpClient
	const freshness = 25 * time.Millisecond
	var initializedAt atomic.Int64
	var receivedTimestamp atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Timestamp time.Time `json:"timestamp"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if initializedAt.Load() == 0 {
			timer := time.NewTimer(4 * freshness)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
				return
			case <-timer.C:
				initializedAt.Store(time.Now().UnixNano())
			}
		}
		receivedTimestamp.Store(body.Timestamp.UnixNano())
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	original := sandboxHttpClient
	sandboxHttpClient = http.Client{}
	t.Cleanup(func() { sandboxHttpClient = original })
	sbx := newTestSandboxWithBundle("")
	sbx.internalConfig.EnvdInitRequestTimeout = freshness
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	response, _, err := sbx.doRequestWithInfiniteRetries(ctx, http.MethodPost, server.URL+"/init")
	if err != nil {
		t.Fatalf("cold initialization was starved by request cancellation: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent || initializedAt.Load() == 0 {
		t.Fatal("guest initialization did not complete")
	}
	if receivedTimestamp.Load() < initializedAt.Load() {
		t.Fatal("accepted the stale timestamp from the slow cold-initialization request")
	}
}

func TestEnvdInitColdSetupRemainsBounded(t *testing.T) { //nolint:paralleltest // overrides sandboxHttpClient
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer server.Close()
	original := sandboxHttpClient
	sandboxHttpClient = http.Client{}
	t.Cleanup(func() { sandboxHttpClient = original })
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	response, _, err := newTestSandboxWithBundle("").doRequestWithInfiniteRetries(ctx, http.MethodPost, server.URL+"/init")
	if response != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("restore deadline was not preserved: response=%v error=%v", response, err)
	}
}

func TestEnvdInitSlowRejectionIsNotRetried(t *testing.T) { //nolint:paralleltest // overrides sandboxHttpClient
	const freshness = 25 * time.Millisecond
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		timer := time.NewTimer(2 * freshness)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("denied"))
		}
	}))
	defer server.Close()
	original := sandboxHttpClient
	sandboxHttpClient = http.Client{}
	t.Cleanup(func() { sandboxHttpClient = original })
	sbx := newTestSandboxWithBundle("")
	sbx.internalConfig.EnvdInitRequestTimeout = freshness
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	response, attempts, err := sbx.doRequestWithInfiniteRetries(ctx, http.MethodPost, server.URL+"/init")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusForbidden || string(body) != "denied" || attempts != 1 {
		t.Fatalf("init rejection was lost or retried: status=%d body=%q attempts=%d error=%v", response.StatusCode, body, attempts, err)
	}
}
