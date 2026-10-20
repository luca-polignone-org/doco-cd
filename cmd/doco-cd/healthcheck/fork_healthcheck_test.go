package healthcheck

import "testing"

// A specific HTTP_BIND_ADDRESS is where the server listens, so the healthcheck
// must use it instead of localhost (fork).
func TestTargetFromEnvBindAddress(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		bind    string
		want    string
		wantErr bool
	}{
		{name: "empty keeps localhost", bind: " ", want: "http://localhost:8164/v1/health"},
		{name: "IPv4 loopback", bind: "127.0.0.1", want: "http://127.0.0.1:8164/v1/health"},
		{name: "trimmed", bind: " 127.0.0.1 ", want: "http://127.0.0.1:8164/v1/health"},
		{name: "IPv6 loopback", bind: "::1", want: "http://[::1]:8164/v1/health"},
		{name: "unspecified IPv4 keeps localhost", bind: "0.0.0.0", want: "http://localhost:8164/v1/health"},
		{name: "unspecified IPv6 keeps localhost", bind: "::", want: "http://localhost:8164/v1/health"},
		{name: "hostname", bind: "localhost", wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			env := map[string]string{"HTTP_PORT": "8164", "HTTP_BIND_ADDRESS": tc.bind}
			lookupEnv := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

			got, err := TargetFromEnv(lookupEnv, "/v1/health")
			if (err != nil) != tc.wantErr {
				t.Fatalf("TargetFromEnv() error = %v, wantErr %v", err, tc.wantErr)
			}

			if !tc.wantErr && got.URL != tc.want {
				t.Errorf("URL = %q, want %q", got.URL, tc.want)
			}
		})
	}
}
