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

// runtimeDirs contengono i socket dei runtime (docker.sock, containerd, rootless in
// /run/user/<uid>): niente che li tocchi può essere montato.
var runtimeDirs = []string{"/run", "/var/run"}

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

	// Se il daemon vede la checkout a un path diverso da quello interno, un symlink
	// assoluto verrebbe risolto nel namespace dell'host e non in quello del container.
	if filepath.Clean(external) != filepath.Clean(internal) {
		if err = rejectAbsoluteSymlinks(internal, rel); err != nil {
			return false
		}
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

var (
	errDanglingSymlink = errors.New("dangling symlink")
	errUnsafeSymlink   = errors.New("unsafe symlink")
	errSymlinkLoop     = errors.New("too many symlinks")
)

const maxSymlinkHops = 40

// rejectAbsoluteSymlinks percorre i componenti di rel sotto root e fallisce al primo
// symlink con destinazione assoluta (anche raggiunto attraverso un symlink relativo).
// Si ferma, senza errore, al primo componente inesistente: il resto lo giudica
// evalExistingPrefix.
func rejectAbsoluteSymlinks(root, rel string) error {
	pending := strings.Split(filepath.ToSlash(rel), "/")
	cur := root

	for hops := 0; len(pending) > 0; {
		comp := pending[0]
		pending = pending[1:]

		switch comp {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}

		next := filepath.Join(cur, comp)

		fi, err := os.Lstat(next)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		if err != nil {
			return err
		}

		if fi.Mode()&os.ModeSymlink == 0 {
			cur = next
			continue
		}

		if hops++; hops > maxSymlinkHops {
			return fmt.Errorf("%w: %s", errSymlinkLoop, next)
		}

		target, err := os.Readlink(next)
		if err != nil || filepath.IsAbs(target) {
			return errors.Join(fmt.Errorf("%w: %s", errUnsafeSymlink, next), err)
		}

		pending = append(strings.Split(filepath.ToSlash(target), "/"), pending...)
	}

	return nil
}

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

// touchesSocket è vero per /run, /var/run, ogni path sotto di essi e ogni loro antenato
// (/, /var): montare un antenato espone i socket dei runtime, qualunque allowlist ci sia.
func touchesSocket(p string) bool {
	for _, d := range runtimeDirs {
		if filesystem.InBasePath(d, p) || filesystem.InBasePath(p, d) {
			return true
		}
	}

	return false
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
