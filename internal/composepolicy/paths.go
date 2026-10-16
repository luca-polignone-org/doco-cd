package composepolicy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/filesystem"
)

var socketPaths = []string{"/var/run/docker.sock", "/run/docker.sock"}
var socketDirs = []string{"/run/containerd", "/var/run/containerd"}

type pathChecker struct {
	paths Paths
	allow []string
}

// inCheckout dice se un path dell'host sta nella checkout del deploy o in quella del
// config source, anche dopo aver risolto i symlink di ogni componente dal lato del
// container (il daemon li segue sull'host).
func (pc pathChecker) inCheckout(hostPath string) bool {
	return within(pc.paths.CheckoutExternal, pc.paths.CheckoutInternal, hostPath) ||
		within(pc.paths.ConfigExternal, pc.paths.ConfigInternal, hostPath)
}

// inCheckoutByPrefix dice se il path pretende di stare in una checkout guardando solo
// il prefisso, senza risolvere symlink.
func (pc pathChecker) inCheckoutByPrefix(hostPath string) bool {
	return (pc.paths.CheckoutExternal != "" && filesystem.InBasePath(pc.paths.CheckoutExternal, hostPath)) ||
		(pc.paths.ConfigExternal != "" && filesystem.InBasePath(pc.paths.ConfigExternal, hostPath))
}

func within(external, internal, hostPath string) bool {
	if external == "" || internal == "" || !filepath.IsAbs(hostPath) {
		return false
	}

	hostPath = filepath.Clean(hostPath)

	rel, err := filepath.Rel(filepath.Clean(external), hostPath)
	if err != nil || !filesystem.InBasePath(external, hostPath) {
		return false
	}

	resolved, err := evalExistingPrefix(filepath.Join(internal, rel))
	if err != nil {
		return false
	}

	root, err := filepath.EvalSymlinks(internal)
	if err != nil {
		return false
	}

	return filesystem.InBasePath(root, resolved)
}

var errDanglingSymlink = errors.New("dangling symlink")

// evalExistingPrefix risolve i symlink del più lungo prefisso esistente e vi riattacca
// il resto, così un path non ancora creato sotto un symlink resta comunque risolto. Un
// symlink pendente è un errore: nel container la destinazione manca, sull'host può
// esistere, e il daemon lo seguirebbe.
func evalExistingPrefix(p string) (string, error) {
	rest := ""

	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}

		// Alla radice non c'è più un genitore da provare: l'errore è definitivo.
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(p) == p {
			return "", err
		}

		if fi, lerr := os.Lstat(p); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: %s", errDanglingSymlink, p)
		}

		rest = filepath.Join(filepath.Base(p), rest)
		p = filepath.Dir(p)
	}
}

func (pc pathChecker) bindAllowed(hostPath string) bool {
	hostPath = filepath.Clean(hostPath)

	if touchesSocket(hostPath) {
		return false
	}

	if pc.inCheckout(hostPath) {
		return true
	}

	// Un path che pretende di stare nella checkout ma ne esce via symlink non può
	// essere salvato dall'allowlist: è proprio il bypass che la regola chiude.
	if pc.inCheckoutByPrefix(hostPath) {
		return false
	}

	for _, a := range pc.allow {
		if filesystem.InBasePath(a, hostPath) {
			return true
		}
	}

	return false
}

// touchesSocket è vero per il socket, per un file *.sock nelle directory dei runtime e
// per ogni antenato di questi path: montare /run espone /run/docker.sock.
func touchesSocket(p string) bool {
	for _, s := range socketPaths {
		if p == s || filesystem.InBasePath(p, s) {
			return true
		}
	}

	for _, d := range socketDirs {
		if filesystem.InBasePath(p, d) || (filesystem.InBasePath(d, p) && strings.HasSuffix(p, ".sock")) {
			return true
		}
	}

	return (strings.HasPrefix(p, "/var/run/") || strings.HasPrefix(p, "/run/")) && strings.HasSuffix(p, ".sock")
}

func bindSource(p *types.Project, vol types.ServiceVolumeConfig) (string, bool) {
	switch vol.Type {
	case types.VolumeTypeBind:
		return vol.Source, true
	case types.VolumeTypeVolume:
		named, ok := p.Volumes[vol.Source]
		if !ok {
			return "", false
		}

		opts := named.DriverOpts
		if dev := opts["device"]; dev != "" && (opts["type"] == "none" || strings.Contains(opts["o"], "bind")) {
			return dev, true
		}
	}

	return "", false
}

func joinsForeignContainer(p *types.Project, mode string) bool {
	kind, name, ok := strings.Cut(mode, ":")
	if !ok {
		return false
	}

	switch kind {
	case "container":
		return true // un container per nome non è mai sotto il controllo del progetto
	case "service":
		return !hasService(p, name)
	default:
		return false
	}
}

func hasService(p *types.Project, name string) bool {
	_, ok := p.Services[name]
	return ok
}

type fileObject struct{ name, file string }

func secretObjects(m types.Secrets) []fileObject {
	out := make([]fileObject, 0, len(m))
	for name, o := range m {
		out = append(out, fileObject{name: name, file: o.File})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })

	return out
}

func configObjects(m types.Configs) []fileObject {
	out := make([]fileObject, 0, len(m))
	for name, o := range m {
		out = append(out, fileObject{name: name, file: o.File})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })

	return out
}

func checkFileObjects(pc pathChecker, kind string, objs []fileObject) []Violation {
	var out []Violation

	for _, o := range objs {
		if o.file != "" && !pc.bindAllowed(o.file) {
			out = append(out, Violation{Field: kind + "." + o.name + ".file", Value: o.file})
		}
	}

	return out
}

func isLocalPath(v string) bool {
	return v != "" && !strings.Contains(v, "://") && !strings.HasPrefix(v, "git@") && !strings.HasPrefix(v, "service:")
}

func filepathIsAbs(p string) bool { return filepath.IsAbs(p) }

func normalizeRepo(u string) string {
	if strings.TrimSpace(u) == "" {
		return ""
	}

	return config.NormalizeGitURL(u)
}
