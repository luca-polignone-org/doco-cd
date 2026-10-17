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

// runtimeDirs hold the runtime sockets (docker.sock, containerd, rootless in
// /run/user/<uid>): nothing that touches them may be mounted.
var runtimeDirs = []string{"/run", "/var/run"}

type pathChecker struct {
	paths Paths
	allow []string
}

// inCheckout reports whether a host path lies in the deploy checkout or in the config
// source one, even after resolving the symlinks of every component on the container
// side (the daemon follows them on the host).
func (pc pathChecker) inCheckout(hostPath string) bool {
	return within(pc.paths.CheckoutExternal, pc.paths.CheckoutInternal, hostPath) ||
		within(pc.paths.ConfigExternal, pc.paths.ConfigInternal, hostPath)
}

// inCheckoutByPrefix reports whether the path claims to be in a checkout by looking
// only at the prefix, without resolving symlinks.
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

	// If the daemon sees the checkout at a different path than the internal one, an
	// absolute symlink would be resolved in the host namespace, not the container one.
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

// rejectAbsoluteSymlinks walks the components of rel under root and fails at the first
// symlink with an absolute target (even one reached through a relative symlink).
// It stops, without error, at the first nonexistent component: evalExistingPrefix
// judges the rest.
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

// evalExistingPrefix resolves the symlinks of the longest existing prefix and
// reattaches the rest, so a path not yet created under a symlink is still resolved. A
// dangling symlink is an error: the target is missing in the container but may exist
// on the host, and the daemon would follow it.
func evalExistingPrefix(p string) (string, error) {
	rest := ""

	for {
		resolved, err := filepath.EvalSymlinks(p)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}

		// At the root there is no parent left to try: the error is final.
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

	// A path that claims to be in the checkout but escapes it via symlink cannot be
	// rescued by the allowlist: that is exactly the bypass this rule closes.
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

// touchesSocket is true for /run, /var/run, any path under them and any of their
// ancestors (/, /var): mounting an ancestor exposes the runtime sockets, whatever the
// allowlist says.
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
		return true // a container by name is never under the project's control
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
