package app

import (
	"sync"

	"github.com/kimdre/doco-cd/internal/config/poll"
)

// Reloaded poll configuration (Portico fork, poli.7). With POLL_CONFIG_FILE reloaded, the poll
// configuration changes while the API, MCP and webhook handlers read it: they read it through
// CurrentPollConfig, and the reload replaces it with SetPollConfig. A slice handed out is never
// modified afterwards: a reload installs a new one.

type livePollConfig struct {
	mu      sync.RWMutex
	configs []poll.Config
}

// EnableLivePollConfig makes the poll configuration replaceable with SetPollConfig, starting from
// PollConfig. It must run before anything reads the configuration concurrently.
func (cfg *Config) EnableLivePollConfig() {
	if cfg.pollConfigLive == nil {
		cfg.pollConfigLive = &livePollConfig{configs: cfg.PollConfig}
	}
}

// CurrentPollConfig returns the poll configuration in force: the last one installed with
// SetPollConfig, or PollConfig. The caller must not modify it.
func (cfg *Config) CurrentPollConfig() []poll.Config {
	live := cfg.pollConfigLive
	if live == nil {
		return cfg.PollConfig
	}

	live.mu.RLock()
	defer live.mu.RUnlock()

	return live.configs
}

// SetPollConfig installs a new poll configuration. Without EnableLivePollConfig it replaces
// PollConfig, which is only safe while nothing reads the configuration concurrently.
func (cfg *Config) SetPollConfig(configs []poll.Config) {
	live := cfg.pollConfigLive
	if live == nil {
		cfg.PollConfig = configs

		return
	}

	live.mu.Lock()
	defer live.mu.Unlock()

	live.configs = configs
}
