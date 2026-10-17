package deploy

import "strings"

// defaultContextName is the name upstream shows for the empty Docker context
// (docker.DefaultContextName); package docker imports deploy, so it is repeated here
// and a test in internal/stages checks that the two match.
const defaultContextName = "default"

// ObjectName is the identity of a target for RBAC, compose policy, hooks and the store:
// <context>/<project>, with the empty context written as "default".
func ObjectName(contextName, project string) string {
	c := strings.TrimSpace(contextName)
	if c == "" || strings.EqualFold(c, defaultContextName) {
		c = defaultContextName
	}

	return c + "/" + strings.TrimSpace(project)
}

// Object returns the object of the target described by the config.
func (c *Config) Object() string {
	return ObjectName(c.Context, c.Name)
}
