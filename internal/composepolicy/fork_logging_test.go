package composepolicy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func loggingFile(t *testing.T, def string) *File {
	t.Helper()

	f, err := Parse([]byte("version: 1\n" + def + "targets:\n  default/web:\n    repository: https://github.com/o/r\n"))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	return f
}

func checkWeb(f *File, svc types.ServiceConfig) []Violation {
	svc.Name = "app"
	p := &types.Project{Name: "web", Services: types.Services{"app": svc}}

	return f.Check(Input{Project: p, Identity: Identity{Object: "default/web", Repository: "https://github.com/o/r"}})
}

func TestLoggingRule(t *testing.T) {
	journald := loggingFile(t, "default:\n  logging:\n    driver: journald\n    options: {}\n")
	none := loggingFile(t, "")

	cases := []struct {
		name string
		f    *File
		svc  types.ServiceConfig
		want []Violation
	}{
		{"no logging", none, types.ServiceConfig{Image: "a"}, nil},
		{
			"logging without default", none,
			types.ServiceConfig{Image: "a", Logging: &types.LoggingConfig{Driver: "journald"}},
			[]Violation{{Service: "app", Field: "logging", Value: "journald"}},
		},
		{"equal to default", journald, types.ServiceConfig{Image: "a", Logging: &types.LoggingConfig{Driver: "journald"}}, nil},
		{
			"other options", journald,
			types.ServiceConfig{Image: "a", Logging: &types.LoggingConfig{Driver: "journald", Options: map[string]string{"env": "X"}}},
			[]Violation{{Service: "app", Field: "logging", Value: "journald"}},
		},
		{
			"other driver", journald,
			types.ServiceConfig{Image: "a", Logging: &types.LoggingConfig{Driver: "json-file"}},
			[]Violation{{Service: "app", Field: "logging", Value: "json-file"}},
		},
		{
			"compose label", none,
			types.ServiceConfig{Image: "a", Labels: types.Labels{"com.docker.compose.project": "gymlog"}},
			[]Violation{{Service: "app", Field: "labels", Value: "com.docker.compose.project"}},
		},
		{
			"upper-case compose label", none,
			types.ServiceConfig{Image: "a", Labels: types.Labels{"COM.DOCKER.COMPOSE.PROJECT": "gymlog"}},
			[]Violation{{Service: "app", Field: "labels", Value: "COM.DOCKER.COMPOSE.PROJECT"}},
		},
		{
			"underscore compose label", none,
			types.ServiceConfig{Image: "a", Labels: types.Labels{"com_docker_compose_service": "x"}},
			[]Violation{{Service: "app", Field: "labels", Value: "com_docker_compose_service"}},
		},
		{
			"middle-dot compose label", none,
			types.ServiceConfig{Image: "a", Labels: types.Labels{"com·docker·compose·project": "gymlog"}},
			[]Violation{{Service: "app", Field: "labels", Value: "com·docker·compose·project"}},
		},
		{
			"empty options against a policy without options", loggingFile(t, "default:\n  logging:\n    driver: journald\n"),
			types.ServiceConfig{Image: "a", Logging: &types.LoggingConfig{Driver: "journald", Options: map[string]string{}}},
			nil,
		},
		{
			"daemon default driver", none,
			types.ServiceConfig{Image: "a", Logging: &types.LoggingConfig{Options: map[string]string{"tag": "x"}}},
			[]Violation{{Service: "app", Field: "logging", Value: "(daemon default)"}},
		},
		{
			"two compose labels, sorted", none,
			types.ServiceConfig{Image: "a", Labels: types.Labels{"com.docker.compose.service": "x", "COM.DOCKER.COMPOSE.PROJECT": "y"}},
			[]Violation{
				{Service: "app", Field: "labels", Value: "COM.DOCKER.COMPOSE.PROJECT"},
				{Service: "app", Field: "labels", Value: "com.docker.compose.service"},
			},
		},
		{"other label", none, types.ServiceConfig{Image: "a", Labels: types.Labels{"org.example.team": "x"}}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := checkWeb(tc.f, tc.svc); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Check() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLoggingOnlyInDefault(t *testing.T) {
	_, err := Parse([]byte("version: 1\ntargets:\n  default/web:\n    repository: https://github.com/o/r\n    logging:\n      driver: journald\n"))
	if err == nil || !strings.Contains(err.Error(), "logging") {
		t.Fatalf("Parse() error = %v, want logging only in default", err)
	}

	for name, def := range map[string]string{
		"empty driver":      "driver: \"\"",
		"padded driver":     "driver: \" journald\"",
		"padded option key": "driver: journald\n    options:\n      \" tag\": x",
		"empty option key":  "driver: journald\n    options:\n      \"\": x",
	} {
		if _, err = Parse([]byte("version: 1\ndefault:\n  logging:\n    " + def + "\n")); err == nil || !strings.Contains(err.Error(), "logging.") {
			t.Fatalf("%s: Parse() error = %v, want a logging error", name, err)
		}
	}

	// A null logging on a target is accepted: targetFor replaces it with default.logging.
	f, err := Parse([]byte("version: 1\ntargets:\n  default/web:\n    repository: https://github.com/o/r\n    logging: ~\n"))
	if err != nil {
		t.Fatalf("Parse() error = %v, want null logging on a target accepted", err)
	}

	if got := checkWeb(f, types.ServiceConfig{Image: "a", Logging: &types.LoggingConfig{Driver: "journald"}}); len(got) != 1 {
		t.Fatalf("Check() = %+v, want the service logging denied", got)
	}
}

func TestNormalizedJournalField(t *testing.T) {
	for in, want := range map[string]string{
		"com.docker.compose.project": "COM_DOCKER_COMPOSE_PROJECT",
		"Com-Docker.compose_x":       "COM_DOCKER_COMPOSE_X",
		"_leading":                   "LEADING",
		"-.leading":                  "LEADING",
		"a__b":                       "A__B",
		"1com.docker":                "1COM_DOCKER",
		// One rune, one "_": a non-ASCII rune is never upper-cased.
		"com·docker·compose·project": "COM_DOCKER_COMPOSE_PROJECT",
		// strings.ToUpper("ſ") is "S": moby keeps it as "_", so this label is not a compose field.
		"com.docker.compoſe.project": "COM_DOCKER_COMPO_E_PROJECT",
	} {
		if got := normalizedJournalField(in); got != want {
			t.Fatalf("normalizedJournalField(%q) = %q, want %q", in, got, want)
		}
	}
}
