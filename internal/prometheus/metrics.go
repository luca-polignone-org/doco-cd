package prometheus

import (
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/kimdre/doco-cd/internal/graceful"
	"github.com/kimdre/doco-cd/internal/logger"
)

const (
	MetricsNamespace = "doco_cd"  // Namespace for all metrics
	MetricsPath      = "/metrics" // Path for exposing metrics via HTTP
)

// RegisterServer registers the Prometheus metrics server.
// bindAddress is the IP to listen on; empty binds every interface.
func RegisterServer(bindAddress string, port uint16, tlsCertFile string, tlsKeyFile string, log *logger.Logger) {
	protocol := "http"
	if tlsCertFile != "" && tlsKeyFile != "" {
		protocol = "https"
	}

	log.Info("serving prometheus metrics",
		slog.Int("http_port", int(port)),
		slog.String("bind_address", bindAddress),
		slog.String("protocol", protocol),
		slog.String("path", MetricsPath),
	)

	mux := http.NewServeMux()
	mux.Handle(MetricsPath, Handler())

	server := &http.Server{
		Addr:              net.JoinHostPort(bindAddress, strconv.Itoa(int(port))),
		ReadHeaderTimeout: 3 * time.Second,
		Handler:           mux,
	}

	graceful.RegisterServer(graceful.NewHttpServer("prometheus", server, tlsCertFile, tlsKeyFile))
}
