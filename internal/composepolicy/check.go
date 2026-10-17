package composepolicy

import (
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/kimdre/doco-cd/internal/git"
)

// defaultCaps is Docker's default capability set: adding them via cap_add widens nothing.
var defaultCaps = []string{
	"CHOWN", "DAC_OVERRIDE", "FSETID", "FOWNER", "MKNOD", "NET_RAW", "SETGID",
	"SETUID", "SETFCAP", "SETPCAP", "NET_BIND_SERVICE", "SYS_CHROOT", "KILL", "AUDIT_WRITE",
}

// forbiddenSecurityOpt forbids any seccomp or AppArmor profile override (even a committed
// file may be permissive), label=disable, label=type:spc_t and systempaths=unconfined.
func forbiddenSecurityOpt(opt string) bool {
	o := strings.ToLower(strings.TrimSpace(opt))
	for _, prefix := range []string{"seccomp=", "seccomp:", "apparmor=", "apparmor:"} {
		if strings.HasPrefix(o, prefix) {
			return true
		}
	}

	switch o {
	case "label=disable", "label:disable", "label=type:spc_t", "label:type:spc_t", "systempaths=unconfined":
		return true
	}

	return false
}

// targetFor returns the effective policy of the target: the default for the lists
// the entry does not declare.
func (f *File) targetFor(object string) (TargetPolicy, bool) {
	tp, ok := f.Targets[object]
	if !ok {
		return TargetPolicy{}, false
	}

	if tp.BindAllowlist == nil {
		tp.BindAllowlist = f.Default.BindAllowlist
	}

	if tp.CapAddAllowlist == nil {
		tp.CapAddAllowlist = f.Default.CapAddAllowlist
	}

	return tp, true
}

func (f *File) Check(in Input) []Violation {
	var out []Violation

	if in.Identity.Swarm {
		out = append(out, Violation{Field: "swarm", Value: "true"})
	}

	tp, ok := f.targetFor(in.Identity.Object)
	if !ok || !identityMatches(tp, in.Identity) {
		// Without an identity bound to the repository there is no point evaluating the rest:
		// the allowlist to apply would belong to an object that is not theirs.
		return append(out, Violation{Field: "target", Value: in.Identity.Object})
	}

	pc := pathChecker{paths: in.Paths, allow: tp.BindAllowlist}

	for _, p := range in.DeployEnvFiles {
		if !pc.inCheckout(p) {
			out = append(out, Violation{Field: "deploy.env_files", Value: p})
		}
	}

	for _, p := range in.SecretsFiles {
		if !pc.inCheckout(p) {
			out = append(out, Violation{Field: "deploy.external_secrets_files", Value: p})
		}
	}

	if in.Project == nil {
		return out
	}

	out = append(out, checkFileObjects(pc, "secrets", secretObjects(in.Project.Secrets))...)
	out = append(out, checkFileObjects(pc, "configs", configObjects(in.Project.Configs))...)

	names := make([]string, 0, len(in.Project.Services))
	for name := range in.Project.Services {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		svc := in.Project.Services[name]
		out = append(out, checkService(in.Project, svc, pc, tp)...)
	}

	return out
}

func identityMatches(tp TargetPolicy, id Identity) bool {
	if tp.Repository != normalizeRepo(id.Repository) {
		return false
	}

	// With a declared reference, an OCI source (empty Reference) does not match.
	if tp.Reference != "" && (id.Reference == "" || !git.ReferenceMatches(id.Reference, tp.Reference)) {
		return false
	}

	return tp.ConfigTarget == "" || tp.ConfigTarget == id.ConfigTarget
}

func checkService(p *types.Project, svc types.ServiceConfig, pc pathChecker, tp TargetPolicy) []Violation {
	var out []Violation

	v := func(field, value string) { out = append(out, Violation{Service: svc.Name, Field: field, Value: value}) }

	if svc.Privileged {
		v("privileged", "true")
	}

	for _, ns := range []struct{ field, value string }{
		{"network_mode", svc.NetworkMode},
		{"pid", svc.Pid},
		{"ipc", svc.Ipc},
		{"uts", svc.Uts},
		{"userns_mode", svc.UserNSMode},
		{"cgroup", svc.Cgroup},
	} {
		if ns.value == "host" || joinsForeignContainer(p, ns.value) {
			v(ns.field, ns.value)
		}
	}

	for _, vf := range svc.VolumesFrom {
		if joinsForeignContainer(p, vf) || (!strings.Contains(vf, ":") && !hasService(p, vf)) {
			v("volumes_from", vf)
		}
	}

	for _, vol := range svc.Volumes {
		if src, isBind := bindSource(p, vol); isBind && !pc.bindAllowed(src) {
			v("volumes", src)
		}
	}

	for _, ef := range svc.EnvFiles {
		if !pc.inCheckout(ef.Path) {
			v("env_file", ef.Path)
		}
	}

	if b := svc.Build; b != nil {
		if isLocalPath(b.Context) && !pc.inCheckout(b.Context) {
			v("build.context", b.Context)
		}

		// A relative Dockerfile is relative to the context: ../../x escapes it.
		if df := b.Dockerfile; df != "" {
			if !filepathIsAbs(df) && isLocalPath(b.Context) {
				df = filepath.Join(b.Context, df)
			}

			if filepathIsAbs(df) && !pc.inCheckout(df) {
				v("build.dockerfile", b.Dockerfile)
			}
		}

		keys := make([]string, 0, len(b.AdditionalContexts))
		for k := range b.AdditionalContexts {
			keys = append(keys, k)
		}

		sort.Strings(keys)

		for _, k := range keys {
			if c := b.AdditionalContexts[k]; isLocalPath(c) && !pc.inCheckout(c) {
				v("build.additional_contexts."+k, c)
			}
		}

		for _, key := range b.SSH {
			if key.Path != "" && !pc.inCheckout(key.Path) {
				v("build.ssh."+key.ID, key.Path)
			}
		}

		if b.Privileged {
			v("build.privileged", "true")
		}

		for _, e := range b.Entitlements {
			v("build.entitlements", e)
		}

		if b.Network == "host" {
			v("build.network", "host")
		}
	}

	for _, c := range svc.CapAdd {
		name := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(c)), "CAP_")
		if name == "ALL" || (!slices.Contains(defaultCaps, name) && !slices.Contains(tp.CapAddAllowlist, name)) {
			v("cap_add", c)
		}
	}

	for _, d := range svc.Devices {
		v("devices", d.Source)
	}

	for _, r := range svc.DeviceCgroupRules {
		v("device_cgroup_rules", r)
	}

	for _, so := range svc.SecurityOpt {
		if forbiddenSecurityOpt(so) {
			v("security_opt", so)
		}
	}

	netNames := make([]string, 0, len(svc.Networks))
	for name := range svc.Networks {
		netNames = append(netNames, name)
	}

	sort.Strings(netNames)

	for _, name := range netNames {
		n, ok := p.Networks[name]
		if !ok || !bool(n.External) {
			continue
		}

		// Without an explicit name, an external network is named after its key.
		effective := n.Name
		if effective == "" {
			effective = name
		}

		if effective == "host" {
			v("networks", name)
		}
	}

	if svc.CgroupParent != "" {
		v("cgroup_parent", svc.CgroupParent)
	}

	return out
}
