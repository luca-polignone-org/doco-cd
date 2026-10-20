package app

import (
	"strings"
	"testing"
)

func TestGetConfigForkBindAddress(t *testing.T) {
	tests := []struct {
		name        string
		http        string
		metrics     string
		wantHTTP    string
		wantMetrics string
		wantErr     string
	}{
		{name: "empty binds every interface"},
		{name: "loopback", http: "127.0.0.1", metrics: "127.0.0.1", wantHTTP: "127.0.0.1", wantMetrics: "127.0.0.1"},
		{name: "trimmed", http: " 127.0.0.1 ", wantHTTP: "127.0.0.1"},
		{name: "IPv6", http: "::1", metrics: "::", wantHTTP: "::1", wantMetrics: "::"},
		{name: "hostname for HTTP", http: "localhost", wantErr: `HTTP_BIND_ADDRESS must be an IP address, got "localhost"`},
		{name: "hostname for metrics", metrics: "n8n.example", wantErr: `METRICS_BIND_ADDRESS must be an IP address, got "n8n.example"`},
		{name: "address with port", http: "127.0.0.1:8080", wantErr: "HTTP_BIND_ADDRESS must be an IP address"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setBaseEnv(t)
			t.Setenv("HTTP_BIND_ADDRESS", tc.http)
			t.Setenv("METRICS_BIND_ADDRESS", tc.metrics)

			cfg, err := GetConfig()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("GetConfig() error = %v, want %q", err, tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("GetConfig() error = %v", err)
			}

			if cfg.HttpBindAddress != tc.wantHTTP || cfg.MetricsBindAddress != tc.wantMetrics {
				t.Errorf("bind = %q, %q, want %q, %q", cfg.HttpBindAddress, cfg.MetricsBindAddress, tc.wantHTTP, tc.wantMetrics)
			}
		})
	}
}
