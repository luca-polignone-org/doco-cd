package composepolicy

import (
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"unicode"

	"github.com/compose-spec/compose-go/v2/types"
)

// The logging rule (Portico fork, spec 5, innesto 3a). A log store that tells apps apart by the
// compose labels the daemon's journald driver attaches only works if an app cannot forge them: a
// service with its own logging (e.g. env=COM_DOCKER_COMPOSE_PROJECT plus a variable of that name,
// which wins over the label) or with a com.docker.compose.* label of its own would sign its lines
// with another project. With a compose policy present every target is checked; services.*.logging
// is allowed only if equal to default.logging.

// LoggingPolicy is the only logging a service may declare (default.logging).
type LoggingPolicy struct {
	Driver  string            `yaml:"driver"`
	Options map[string]string `yaml:"options"`
}

var errLoggingInTarget = errors.New("logging is only allowed in default")

// validateLoggingPolicy refuses a default.logging that no service could ever match: an empty or
// padded driver, or a padded option key (services are compared verbatim, and compose does not trim
// either). Option values are compared verbatim too, so a padded value is matchable and is kept.
func validateLoggingPolicy(lp *LoggingPolicy) error {
	if lp == nil {
		return nil
	}

	if strings.TrimSpace(lp.Driver) == "" {
		return errors.New("logging.driver is required")
	}

	if lp.Driver != strings.TrimSpace(lp.Driver) {
		return fmt.Errorf("logging.driver %q has leading or trailing whitespace", lp.Driver)
	}

	for k := range lp.Options {
		if k != strings.TrimSpace(k) || k == "" {
			return fmt.Errorf("logging.options key %q is empty or has leading or trailing whitespace", k)
		}
	}

	return nil
}

// normalizedJournalField is the journal field name the journald log driver derives from a label
// name. It is a verbatim copy of sanitizeKeyMod (moby daemon/logger/journald/journald.go): per
// rune, a-z is upper-cased, anything outside A-Z and 0-9 becomes "_", leading "_" are skipped.
func normalizedJournalField(s string) string {
	n := ""

	for _, v := range s {
		if 'a' <= v && v <= 'z' {
			v = unicode.ToUpper(v)
		} else if ('Z' < v || v < 'A') && ('9' < v || v < '0') {
			v = '_'
		}

		if n == "" && v == '_' {
			// skip leading underscores
			continue
		}

		n += string(v)
	}

	return n
}

func checkLogging(svc types.ServiceConfig, want *LoggingPolicy) []Violation {
	var out []Violation

	if l := svc.Logging; l != nil && (want == nil || l.Driver != want.Driver || !maps.Equal(nonNil(l.Options), nonNil(want.Options))) {
		driver := l.Driver
		if driver == "" {
			driver = "(daemon default)"
		}

		out = append(out, Violation{Service: svc.Name, Field: "logging", Value: driver})
	}

	keys := make([]string, 0, len(svc.Labels))
	for k := range svc.Labels {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		if strings.HasPrefix(normalizedJournalField(k), "COM_DOCKER_COMPOSE_") {
			out = append(out, Violation{Service: svc.Name, Field: "labels", Value: k})
		}
	}

	return out
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}

	return m
}
