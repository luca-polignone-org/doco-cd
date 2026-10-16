package deploy

import "strings"

// defaultContextName è il nome che upstream mostra per il context Docker vuoto
// (docker.DefaultContextName); il pacchetto docker importa deploy, quindi qui è ripetuto
// e un test in internal/stages verifica che coincidano.
const defaultContextName = "default"

// ObjectName è l'identità di un target per RBAC, compose policy, hook e store:
// <context>/<project>, con il context vuoto scritto "default".
func ObjectName(contextName, project string) string {
	c := strings.TrimSpace(contextName)
	if c == "" || strings.EqualFold(c, defaultContextName) {
		c = defaultContextName
	}

	return c + "/" + strings.TrimSpace(project)
}

// Object restituisce l'oggetto del target descritto dalla config.
func (c *Config) Object() string {
	return ObjectName(c.Context, c.Name)
}
