package main

import (
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/kimdre/doco-cd/internal/api"
	jwtauth "github.com/kimdre/doco-cd/internal/auth/jwt"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/graceful"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/rbac"
	"github.com/kimdre/doco-cd/internal/restapi"
)

// newForkAuth builds the API authenticator and the RBAC enforcer from the fork keys. Without
// them it returns the upstream x-api-key check and a nil enforcer (RBAC off).
func newForkAuth(c *app.Config, log *logger.Logger) (*restapi.Authenticator, *rbac.Enforcer, error) {
	// A nil *jwtauth.Verifier stored in the interface would not be a nil interface, so
	// verifier is assigned only when a verifier exists.
	var verifier restapi.TokenVerifier

	if c.JWTJWKSURL != "" {
		v, err := jwtauth.NewVerifier(jwtauth.Config{
			JWKSURL: c.JWTJWKSURL, Issuer: c.JWTIssuer,
			Audience: c.JWTAudience, GroupsClaim: c.JWTGroupsClaim,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("create JWT verifier: %w", err)
		}

		verifier = v
	}

	var enforcer *rbac.Enforcer

	if c.RBACPolicyFile != "" {
		e, err := rbac.NewFileEnforcer(c.RBACPolicyFile, log.Logger)
		if err != nil {
			return nil, nil, fmt.Errorf("load RBAC policy: %w", err)
		}

		enforcer = e
	}

	return restapi.NewAuthenticator(c, verifier), enforcer, nil
}

func registryApiServer(c *app.Config, h *api.Handler, mounts api.Mounts, log *logger.Logger) error {
	// Register API endpoints
	apiServerMux := http.NewServeMux()

	enabledApiEndpoints, err := api.RegisterRoutes(apiServerMux, h, mounts)
	if err != nil {
		return fmt.Errorf("register API routes: %w", err)
	}

	protocol := "http"
	if c.HttpTLSEnabled {
		protocol = "https"
	}

	log.Info(
		"listening for events",
		slog.Int("http_port", int(c.HttpPort)),
		slog.String("bind_address", c.HttpBindAddress),
		slog.String("protocol", protocol),
		slog.Any("enabled_endpoints", enabledApiEndpoints),
	)

	server := &http.Server{
		Addr:              net.JoinHostPort(c.HttpBindAddress, strconv.Itoa(int(c.HttpPort))),
		ReadHeaderTimeout: 3 * time.Second,
		// ReadTimeout bounds the time to read the request (headers + body) to
		// mitigate slow-body (Slowloris) attacks. The webhook body is already
		// capped via MaxBytesReader, so a generous limit is sufficient.
		ReadTimeout: 30 * time.Second,
		// IdleTimeout bounds how long keep-alive connections may stay idle.
		IdleTimeout: 120 * time.Second,
		// WriteTimeout is intentionally left unset (0): synchronous deployments
		// triggered via ?wait=true can legitimately run for several minutes.
		Handler: apiServerMux,
	}

	graceful.RegisterServer(graceful.NewHttpServer("api", server, c.HttpTLSCertFile, c.HttpTLSKeyFile))

	return nil
}
