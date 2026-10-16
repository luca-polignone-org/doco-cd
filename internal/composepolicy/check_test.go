package composepolicy

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func project(services ...types.ServiceConfig) *types.Project {
	p := &types.Project{Name: "web", Services: types.Services{}}
	for _, s := range services {
		p.Services[s.Name] = s
	}

	return p
}

func bind(src string) types.ServiceVolumeConfig {
	return types.ServiceVolumeConfig{Type: types.VolumeTypeBind, Source: src, Target: "/x"}
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()

	if err := os.MkdirAll(p, 0o750); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()

	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func vi(field, value string) Violation { return Violation{Service: "app", Field: field, Value: value} }

func pv(field, value string) Violation { return Violation{Field: field, Value: value} }

type checkCase struct {
	name string
	tp   *TargetPolicy // sostituisce la voce di base
	def  TargetPolicy
	mod  func(*Input)
	want []Violation
}

// app imposta il progetto con un solo servizio "app" modificato da f.
func app(f func(*types.ServiceConfig)) func(*Input) {
	return func(in *Input) {
		s := types.ServiceConfig{Name: "app"}
		f(&s)
		in.Project = project(s)
	}
}

func TestCheck(t *testing.T) {
	ext := "/host/data/repos/x"
	intl := t.TempDir()
	cfgExt := "/host/data/repos/cfg"
	cfgInt := t.TempDir()

	mustSymlink(t, "/etc", filepath.Join(intl, "etc"))
	gone := t.TempDir()
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	mustSymlink(t, filepath.Join(gone, "secret"), filepath.Join(intl, "x"))
	mustMkdir(t, filepath.Join(intl, "foo"))
	mustSymlink(t, filepath.Join(intl, "foo"), filepath.Join(intl, "s"))
	mustSymlink(t, "loop", filepath.Join(intl, "loop"))
	mustSymlink(t, "./sub/", filepath.Join(intl, "dot"))
	mustSymlink(t, "../../../nope", filepath.Join(intl, "y"))
	mustMkdir(t, filepath.Join(intl, "sub"))
	mustSymlink(t, "sub", filepath.Join(intl, "inner"))

	if err := os.WriteFile(filepath.Join(intl, "plain"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	mustSymlink(t, "/etc", filepath.Join(cfgInt, "etc"))

	allow := func(paths ...string) *TargetPolicy {
		return &TargetPolicy{Repository: "https://github.com/o/r", Reference: "main", BindAllowlist: paths}
	}
	caps := func(names ...string) *TargetPolicy {
		return &TargetPolicy{Repository: "https://github.com/o/r", Reference: "main", CapAddAllowlist: names}
	}
	ident := func(f func(*Identity)) func(*Input) {
		return func(in *Input) { f(&in.Identity) }
	}
	volOnly := func(vols ...types.ServiceVolumeConfig) func(*Input) {
		return app(func(s *types.ServiceConfig) { s.Volumes = vols })
	}
	build := func(b types.BuildConfig) func(*Input) {
		return app(func(s *types.ServiceConfig) { s.Build = &b })
	}
	secOpt := func(o string) checkCase {
		return checkCase{
			name: "security_opt " + o,
			mod:  app(func(s *types.ServiceConfig) { s.SecurityOpt = []string{o} }),
			want: []Violation{vi("security_opt", o)},
		}
	}

	cases := []checkCase{
		{name: "identity ok", want: nil},
		{name: "unknown object", mod: ident(func(i *Identity) { i.Object = "noi/other" }), want: []Violation{pv("target", "noi/other")}},
		{
			name: "different repository",
			mod:  ident(func(i *Identity) { i.Repository = "https://github.com/o/evil" }),
			want: []Violation{pv("target", "noi/web")},
		},
		{
			name: "empty repository",
			mod:  ident(func(i *Identity) { i.Repository = "  " }),
			want: []Violation{pv("target", "noi/web")},
		},
		{
			name: "different reference",
			mod:  ident(func(i *Identity) { i.Reference = "refs/heads/dev" }),
			want: []Violation{pv("target", "noi/web")},
		},
		{name: "oci with declared reference", mod: ident(func(i *Identity) { i.Reference = "" }), want: []Violation{pv("target", "noi/web")}},
		{
			name: "no declared reference accepts oci",
			tp:   &TargetPolicy{Repository: "https://github.com/o/r"},
			mod:  ident(func(i *Identity) { i.Reference = "" }),
			want: nil,
		},
		{
			name: "different config_target",
			tp:   &TargetPolicy{Repository: "https://github.com/o/r", Reference: "main", ConfigTarget: "a"},
			mod:  ident(func(i *Identity) { i.ConfigTarget = "b" }),
			want: []Violation{pv("target", "noi/web")},
		},
		{
			name: "same config_target",
			tp:   &TargetPolicy{Repository: "https://github.com/o/r", Reference: "main", ConfigTarget: "a"},
			mod:  ident(func(i *Identity) { i.ConfigTarget = "a" }),
			want: nil,
		},
		{
			name: "tag as reference",
			tp:   &TargetPolicy{Repository: "https://github.com/o/r", Reference: "refs/tags/v1.0.0"},
			mod:  ident(func(i *Identity) { i.Reference = "refs/tags/v1.0.0" }),
			want: nil,
		},
		{
			name: "short name does not match a tag",
			tp:   &TargetPolicy{Repository: "https://github.com/o/r", Reference: "main"},
			mod:  ident(func(i *Identity) { i.Reference = "refs/tags/main" }),
			want: []Violation{pv("target", "noi/web")},
		},
		{
			name: "other tag",
			tp:   &TargetPolicy{Repository: "https://github.com/o/r", Reference: "refs/tags/v1.0.0"},
			mod:  ident(func(i *Identity) { i.Reference = "refs/tags/v2.0.0" }),
			want: []Violation{pv("target", "noi/web")},
		},
		{name: "swarm", mod: ident(func(i *Identity) { i.Swarm = true }), want: []Violation{pv("swarm", "true")}},
		{
			name: "swarm and unknown object",
			mod:  ident(func(i *Identity) { i.Swarm = true; i.Object = "noi/other" }),
			want: []Violation{pv("swarm", "true"), pv("target", "noi/other")},
		},
		{
			name: "nil project",
			mod:  func(in *Input) { in.Project = nil; in.DeployEnvFiles = []string{"/etc/env"} },
			want: []Violation{pv("deploy.env_files", "/etc/env")},
		},

		{name: "privileged", mod: app(func(s *types.ServiceConfig) { s.Privileged = true }), want: []Violation{vi("privileged", "true")}},
		{name: "network host", mod: app(func(s *types.ServiceConfig) { s.NetworkMode = "host" }), want: []Violation{vi("network_mode", "host")}},
		{name: "pid host", mod: app(func(s *types.ServiceConfig) { s.Pid = "host" }), want: []Violation{vi("pid", "host")}},
		{name: "ipc host", mod: app(func(s *types.ServiceConfig) { s.Ipc = "host" }), want: []Violation{vi("ipc", "host")}},
		{name: "uts host", mod: app(func(s *types.ServiceConfig) { s.Uts = "host" }), want: []Violation{vi("uts", "host")}},
		{name: "userns host", mod: app(func(s *types.ServiceConfig) { s.UserNSMode = "host" }), want: []Violation{vi("userns_mode", "host")}},
		{name: "cgroup host", mod: app(func(s *types.ServiceConfig) { s.Cgroup = "host" }), want: []Violation{vi("cgroup", "host")}},
		{name: "pid private", mod: app(func(s *types.ServiceConfig) { s.Pid = "private" }), want: nil},
		{
			name: "foreign container",
			mod:  app(func(s *types.ServiceConfig) { s.NetworkMode = "container:other" }),
			want: []Violation{vi("network_mode", "container:other")},
		},
		{
			name: "internal service",
			mod: func(in *Input) {
				in.Project = project(
					types.ServiceConfig{Name: "app", NetworkMode: "service:db"},
					types.ServiceConfig{Name: "db"},
				)
			},
			want: nil,
		},
		{
			name: "unknown service",
			mod:  app(func(s *types.ServiceConfig) { s.Pid = "service:ghost" }),
			want: []Violation{vi("pid", "service:ghost")},
		},
		{
			name: "volumes_from foreign container",
			mod:  app(func(s *types.ServiceConfig) { s.VolumesFrom = []string{"container:other"} }),
			want: []Violation{vi("volumes_from", "container:other")},
		},
		{
			name: "volumes_from unknown service",
			mod:  app(func(s *types.ServiceConfig) { s.VolumesFrom = []string{"ghost"} }),
			want: []Violation{vi("volumes_from", "ghost")},
		},
		{
			name: "volumes_from internal service",
			mod: func(in *Input) {
				in.Project = project(
					types.ServiceConfig{Name: "app", VolumesFrom: []string{"db", "db:ro", "service:db"}},
					types.ServiceConfig{Name: "db"},
				)
			},
			want: nil,
		},

		{name: "bind in checkout", mod: volOnly(bind(ext + "/conf")), want: nil},
		{name: "bind in checkout via internal symlink", mod: volOnly(bind(ext + "/inner/f")), want: nil},
		{name: "bind outside", mod: volOnly(bind("/etc")), want: []Violation{vi("volumes", "/etc")}},
		{name: "relative bind", mod: volOnly(bind("data")), want: []Violation{vi("volumes", "data")}},
		{name: "prefix is not boundary", tp: allow("/srv/a"), mod: volOnly(bind("/srv/ab")), want: []Violation{vi("volumes", "/srv/ab")}},
		{name: "bind in allowlist", tp: allow("/srv/a"), mod: volOnly(bind("/srv/a/x")), want: nil},
		{name: "final symlink", mod: volOnly(bind(ext + "/etc")), want: []Violation{vi("volumes", ext+"/etc")}},
		{name: "parent symlink", mod: volOnly(bind(ext + "/etc/shadow")), want: []Violation{vi("volumes", ext+"/etc/shadow")}},
		{
			name: "missing path under symlink",
			mod:  volOnly(bind(ext + "/etc/nope/deeper")),
			want: []Violation{vi("volumes", ext+"/etc/nope/deeper")},
		},
		{
			name: "symlink escape is not saved by allowlist",
			tp:   allow(ext),
			mod:  volOnly(bind(ext + "/etc")),
			want: []Violation{vi("volumes", ext+"/etc")},
		},
		{
			name: "dangling symlink",
			mod:  volOnly(bind(ext+"/x"), bind(ext+"/x/sub")),
			want: []Violation{vi("volumes", ext+"/x"), vi("volumes", ext+"/x/sub")},
		},
		{name: "absolute symlink inside checkout", mod: volOnly(bind(ext + "/s")), want: []Violation{vi("volumes", ext+"/s")}},
		{name: "absolute symlink in the middle", mod: volOnly(bind(ext + "/s/f")), want: []Violation{vi("volumes", ext+"/s/f")}},
		{name: "symlink loop", mod: volOnly(bind(ext + "/loop")), want: []Violation{vi("volumes", ext+"/loop")}},
		{name: "relative symlink with dot components", mod: volOnly(bind(ext + "/dot/f")), want: nil},
		{
			name: "absolute symlink with same external and internal path",
			mod: func(in *Input) {
				in.Paths = Paths{CheckoutExternal: intl, CheckoutInternal: intl}
				volOnly(bind(intl + "/s"))(in)
			},
			want: nil,
		},
		{name: "relative symlink escaping", mod: volOnly(bind(ext + "/y/z")), want: []Violation{vi("volumes", ext+"/y/z")}},
		{name: "path through a file", mod: volOnly(bind(ext + "/plain/sub")), want: []Violation{vi("volumes", ext+"/plain/sub")}},
		{
			name: "checkout internal missing",
			mod: func(in *Input) {
				in.Paths.CheckoutInternal = filepath.Join(intl, "gone")
				volOnly(bind(ext + "/conf"))(in)
			},
			want: []Violation{vi("volumes", ext+"/conf")},
		},
		{
			name: "config source env file",
			mod: func(in *Input) {
				in.Paths.ConfigExternal, in.Paths.ConfigInternal = cfgExt, cfgInt
				in.DeployEnvFiles = []string{cfgExt + "/.env"}
			},
			want: nil,
		},
		{
			name: "config source symlink escape",
			mod: func(in *Input) {
				in.Paths.ConfigExternal, in.Paths.ConfigInternal = cfgExt, cfgInt
				volOnly(bind(cfgExt + "/etc"))(in)
			},
			want: []Violation{vi("volumes", cfgExt+"/etc")},
		},
		{
			name: "dockerfile relative escaping",
			mod:  build(types.BuildConfig{Context: ext + "/app", Dockerfile: "../../../etc/x"}),
			want: []Violation{vi("build.dockerfile", "../../../etc/x")},
		},
		{name: "dockerfile relative inside", mod: build(types.BuildConfig{Context: ext + "/app", Dockerfile: "Dockerfile"}), want: nil},
		{name: "dockerfile absolute inside", mod: build(types.BuildConfig{Context: ext, Dockerfile: ext + "/Dockerfile"}), want: nil},
		{
			name: "dockerfile absolute outside",
			mod:  build(types.BuildConfig{Context: ext, Dockerfile: "/etc/x"}),
			want: []Violation{vi("build.dockerfile", "/etc/x")},
		},
		{
			name: "dockerfile relative with remote context",
			mod:  build(types.BuildConfig{Context: "https://github.com/o/r.git", Dockerfile: "../x"}),
			want: nil,
		},
		{
			name: "build.ssh",
			mod:  build(types.BuildConfig{Context: ext, SSH: types.SSHConfig{{ID: "default", Path: "/root/.ssh/id_rsa"}}}),
			want: []Violation{vi("build.ssh.default", "/root/.ssh/id_rsa")},
		},
		{name: "build.ssh agent", mod: build(types.BuildConfig{Context: ext, SSH: types.SSHConfig{{ID: "default"}}}), want: nil},
		{name: "build.ssh in checkout", mod: build(types.BuildConfig{Context: ext, SSH: types.SSHConfig{{ID: "k", Path: ext + "/key"}}}), want: nil},
		{
			name: "build.privileged",
			mod:  build(types.BuildConfig{Context: ext, Privileged: true}),
			want: []Violation{vi("build.privileged", "true")},
		},
		{
			name: "build.entitlements",
			mod:  build(types.BuildConfig{Context: ext, Entitlements: []string{"security.insecure"}}),
			want: []Violation{vi("build.entitlements", "security.insecure")},
		},
		{
			name: "build.network host",
			mod:  build(types.BuildConfig{Context: ext, Network: "host"}),
			want: []Violation{vi("build.network", "host")},
		},
		{name: "build.network other", mod: build(types.BuildConfig{Context: ext, Network: "none"}), want: nil},
		{name: "build context outside", mod: build(types.BuildConfig{Context: "/"}), want: []Violation{vi("build.context", "/")}},
		{name: "build context url", mod: build(types.BuildConfig{Context: "https://github.com/o/r.git"}), want: nil},
		{name: "build context git ssh", mod: build(types.BuildConfig{Context: "git@github.com:o/r.git"}), want: nil},
		{
			name: "additional_contexts",
			mod: build(types.BuildConfig{Context: ext, AdditionalContexts: types.Mapping{
				"b": "docker-image://alpine", "a": "/etc", "c": "service:other", "d": "/var",
			}}),
			want: []Violation{vi("build.additional_contexts.a", "/etc"), vi("build.additional_contexts.d", "/var")},
		},

		{
			name: "device_cgroup_rules",
			mod:  app(func(s *types.ServiceConfig) { s.DeviceCgroupRules = []string{"c *:* rwm"} }),
			want: []Violation{vi("device_cgroup_rules", "c *:* rwm")},
		},
		secOpt("seccomp=./profile.json"),
		secOpt("seccomp=unconfined"),
		secOpt("seccomp:unconfined"),
		secOpt("apparmor=unconfined"),
		secOpt("apparmor:unconfined"),
		secOpt("label=disable"),
		secOpt("label:disable"),
		secOpt("systempaths=unconfined"),
		secOpt("label=type:spc_t"),
		secOpt("label:type:spc_t"),
		secOpt("LABEL=TYPE:SPC_T"),
		{name: "security_opt allowed", mod: app(func(s *types.ServiceConfig) { s.SecurityOpt = []string{"no-new-privileges:true"} }), want: nil},
		{
			name: "external host network",
			mod: func(in *Input) {
				app(func(s *types.ServiceConfig) { s.Networks = map[string]*types.ServiceNetworkConfig{"h": nil} })(in)
				in.Project.Networks = types.Networks{"h": {Name: "host", External: true}}
			},
			want: []Violation{vi("networks", "h")},
		},
		{
			name: "external network named by key",
			mod: func(in *Input) {
				app(func(s *types.ServiceConfig) { s.Networks = map[string]*types.ServiceNetworkConfig{"host": nil} })(in)
				in.Project.Networks = types.Networks{"host": {External: true}}
			},
			want: []Violation{vi("networks", "host")},
		},
		{
			name: "networks in key order",
			mod: func(in *Input) {
				app(func(s *types.ServiceConfig) {
					s.Networks = map[string]*types.ServiceNetworkConfig{"h2": nil, "h1": nil, "ok": nil, "ghost": nil}
				})(in)
				in.Project.Networks = types.Networks{
					"h1":  {Name: "host", External: true},
					"h2":  {Name: "host", External: true},
					"ok":  {Name: "shared", External: true},
					"int": {Name: "host"},
				}
			},
			want: []Violation{vi("networks", "h1"), vi("networks", "h2")},
		},
		{
			name: "internal network named host",
			mod: func(in *Input) {
				app(func(s *types.ServiceConfig) { s.Networks = map[string]*types.ServiceNetworkConfig{"h": nil} })(in)
				in.Project.Networks = types.Networks{"h": {Name: "host"}}
			},
			want: nil,
		},

		{
			name: "docker socket",
			tp:   allow("/var/run"),
			mod:  volOnly(bind("/var/run/docker.sock")),
			want: []Violation{vi("volumes", "/var/run/docker.sock")},
		},
		{name: "socket ancestor", tp: allow("/"), mod: volOnly(bind("/run")), want: []Violation{vi("volumes", "/run")}},
		{
			name: "socket in /run",
			tp:   allow("/run"),
			mod:  volOnly(bind("/run/docker.sock")),
			want: []Violation{vi("volumes", "/run/docker.sock")},
		},
		{
			name: "containerd socket",
			tp:   allow("/run"),
			mod:  volOnly(bind("/run/containerd/containerd.sock")),
			want: []Violation{vi("volumes", "/run/containerd/containerd.sock")},
		},
		{
			name: "containerd directory",
			tp:   allow("/"),
			mod:  volOnly(bind("/var/run/containerd")),
			want: []Violation{vi("volumes", "/var/run/containerd")},
		},
		{
			name: "other runtime socket",
			tp:   allow("/run"),
			mod:  volOnly(bind("/run/foo.sock")),
			want: []Violation{vi("volumes", "/run/foo.sock")},
		},
		{
			name: "any path in /run",
			tp:   allow("/run"),
			mod:  volOnly(bind("/run/app/data")),
			want: []Violation{vi("volumes", "/run/app/data")},
		},
		{
			name: "rootless docker directory",
			tp:   allow("/run/user"),
			mod:  volOnly(bind("/run/user/1000")),
			want: []Violation{vi("volumes", "/run/user/1000")},
		},
		{
			name: "any path in /var/run",
			tp:   allow("/var"),
			mod:  volOnly(bind("/var/run/x")),
			want: []Violation{vi("volumes", "/var/run/x")},
		},
		{name: "var ancestor", tp: allow("/"), mod: volOnly(bind("/var")), want: []Violation{vi("volumes", "/var")}},
		{name: "root ancestor", tp: allow("/"), mod: volOnly(bind("/")), want: []Violation{vi("volumes", "/")}},
		{name: "similar prefix is not run", tp: allow("/"), mod: volOnly(bind("/runner")), want: nil},
		{name: "sock elsewhere", tp: allow("/srv"), mod: volOnly(bind("/srv/app.sock")), want: nil},

		{
			name: "named volume with bind option",
			mod: func(in *Input) {
				volOnly(types.ServiceVolumeConfig{Type: types.VolumeTypeVolume, Source: "data"})(in)
				in.Project.Volumes = types.Volumes{"data": {
					Driver: "local", DriverOpts: map[string]string{"type": "none", "o": "bind", "device": "/etc"},
				}}
			},
			want: []Violation{vi("volumes", "/etc")},
		},
		{
			name: "named volume with type none",
			mod: func(in *Input) {
				volOnly(types.ServiceVolumeConfig{Type: types.VolumeTypeVolume, Source: "data"})(in)
				in.Project.Volumes = types.Volumes{"data": {DriverOpts: map[string]string{"type": "none", "device": "/etc"}}}
			},
			want: []Violation{vi("volumes", "/etc")},
		},
		{
			name: "named volume with o=bind only",
			mod: func(in *Input) {
				volOnly(types.ServiceVolumeConfig{Type: types.VolumeTypeVolume, Source: "data"})(in)
				in.Project.Volumes = types.Volumes{"data": {DriverOpts: map[string]string{"o": "rbind", "device": "/etc"}}}
			},
			want: []Violation{vi("volumes", "/etc")},
		},
		{
			name: "named volume with device but no bind",
			mod: func(in *Input) {
				volOnly(types.ServiceVolumeConfig{Type: types.VolumeTypeVolume, Source: "data"})(in)
				in.Project.Volumes = types.Volumes{"data": {DriverOpts: map[string]string{"type": "nfs", "device": ":/exports"}}}
			},
			want: nil,
		},
		{
			name: "named volume plain",
			mod: func(in *Input) {
				volOnly(types.ServiceVolumeConfig{Type: types.VolumeTypeVolume, Source: "data"})(in)
				in.Project.Volumes = types.Volumes{"data": {}}
			},
			want: nil,
		},
		{
			name: "named volume undeclared",
			mod:  volOnly(types.ServiceVolumeConfig{Type: types.VolumeTypeVolume, Source: "data"}),
			want: nil,
		},
		{name: "tmpfs volume", mod: volOnly(types.ServiceVolumeConfig{Type: types.VolumeTypeTmpfs, Target: "/t"}), want: nil},

		{
			name: "secret file",
			mod: func(in *Input) {
				in.Project = project()
				in.Project.Secrets = types.Secrets{"s": {File: "/etc/shadow"}}
			},
			want: []Violation{pv("secrets.s.file", "/etc/shadow")},
		},
		{
			name: "secret from environment",
			mod: func(in *Input) {
				in.Project = project()
				in.Project.Secrets = types.Secrets{"s": {Environment: "TOKEN"}}
			},
			want: nil,
		},
		{
			name: "config file in checkout",
			mod: func(in *Input) {
				in.Project = project()
				in.Project.Configs = types.Configs{"c": {File: ext + "/c.conf"}}
			},
			want: nil,
		},
		{
			name: "config file outside",
			mod: func(in *Input) {
				in.Project = project()
				in.Project.Configs = types.Configs{"c": {File: "/etc/passwd"}}
			},
			want: []Violation{pv("configs.c.file", "/etc/passwd")},
		},
		{
			name: "env_file outside",
			mod:  app(func(s *types.ServiceConfig) { s.EnvFiles = []types.EnvFile{{Path: "/data/fork/x"}} }),
			want: []Violation{vi("env_file", "/data/fork/x")},
		},
		{name: "env_file inside", mod: app(func(s *types.ServiceConfig) { s.EnvFiles = []types.EnvFile{{Path: ext + "/.env"}} }), want: nil},
		{
			name: "env_file in allowlist",
			tp:   allow("/srv/a"),
			mod:  app(func(s *types.ServiceConfig) { s.EnvFiles = []types.EnvFile{{Path: "/srv/a/e"}} }),
			want: []Violation{vi("env_file", "/srv/a/e")},
		},
		{
			name: "deploy env_files",
			mod:  func(in *Input) { in.DeployEnvFiles = []string{"/etc/env", ext + "/.env"} },
			want: []Violation{pv("deploy.env_files", "/etc/env")},
		},
		{
			name: "external_secrets_files",
			mod:  func(in *Input) { in.SecretsFiles = []string{"/etc/x"} },
			want: []Violation{pv("deploy.external_secrets_files", "/etc/x")},
		},

		{
			name: "default caps",
			mod:  app(func(s *types.ServiceConfig) { s.CapAdd = []string{"CHOWN", "cap_net_bind_service"} }),
			want: nil,
		},
		{
			name: "cap outside default",
			mod:  app(func(s *types.ServiceConfig) { s.CapAdd = []string{"SYS_ADMIN"} }),
			want: []Violation{vi("cap_add", "SYS_ADMIN")},
		},
		{
			name: "cap in target allowlist",
			tp:   caps("NET_ADMIN"),
			mod:  app(func(s *types.ServiceConfig) { s.CapAdd = []string{"NET_ADMIN"} }),
			want: nil,
		},
		{
			name: "cap in target allowlist with prefix",
			tp:   caps("NET_ADMIN"),
			mod:  app(func(s *types.ServiceConfig) { s.CapAdd = []string{" cap_net_admin "} }),
			want: nil,
		},
		{
			name: "cap ALL",
			tp:   caps("ALL"),
			mod:  app(func(s *types.ServiceConfig) { s.CapAdd = []string{"ALL"} }),
			want: []Violation{vi("cap_add", "ALL")},
		},
		{
			name: "devices",
			mod:  app(func(s *types.ServiceConfig) { s.Devices = []types.DeviceMapping{{Source: "/dev/kvm"}} }),
			want: []Violation{vi("devices", "/dev/kvm")},
		},
		{
			name: "cgroup_parent",
			mod:  app(func(s *types.ServiceConfig) { s.CgroupParent = "x" }),
			want: []Violation{vi("cgroup_parent", "x")},
		},

		{
			name: "default inherited",
			tp:   &TargetPolicy{Repository: "https://github.com/o/r", Reference: "main"},
			def:  TargetPolicy{BindAllowlist: []string{"/srv/shared"}, CapAddAllowlist: []string{"NET_ADMIN"}},
			mod: app(func(s *types.ServiceConfig) {
				s.Volumes = []types.ServiceVolumeConfig{bind("/srv/shared/x")}
				s.CapAdd = []string{"NET_ADMIN"}
			}),
			want: nil,
		},
		{
			name: "target with own allowlist",
			tp:   allow("/srv/a"),
			def:  TargetPolicy{BindAllowlist: []string{"/srv/shared"}},
			mod:  volOnly(bind("/srv/shared/x")),
			want: []Violation{vi("volumes", "/srv/shared/x")},
		},
		{
			name: "target with own cap allowlist",
			tp:   caps("CHOWN"),
			def:  TargetPolicy{CapAddAllowlist: []string{"NET_ADMIN"}},
			mod:  app(func(s *types.ServiceConfig) { s.CapAdd = []string{"NET_ADMIN"} }),
			want: []Violation{vi("cap_add", "NET_ADMIN")},
		},
		{
			name: "order of rules and services",
			mod: func(in *Input) {
				in.Identity.Swarm = true
				in.DeployEnvFiles = []string{"/e1"}
				in.SecretsFiles = []string{"/e2"}
				in.Project = project(
					types.ServiceConfig{Name: "b", Privileged: true},
					types.ServiceConfig{Name: "a", Privileged: true, NetworkMode: "host", CgroupParent: "x"},
				)
				in.Project.Secrets = types.Secrets{"z": {File: "/s2"}, "y": {File: "/s1"}}
				in.Project.Configs = types.Configs{"c": {File: "/c1"}, "a": {File: "/c0"}}
			},
			want: []Violation{
				pv("swarm", "true"),
				pv("deploy.env_files", "/e1"),
				pv("deploy.external_secrets_files", "/e2"),
				pv("secrets.y.file", "/s1"),
				pv("secrets.z.file", "/s2"),
				pv("configs.a.file", "/c0"),
				pv("configs.c.file", "/c1"),
				{Service: "a", Field: "privileged", Value: "true"},
				{Service: "a", Field: "network_mode", Value: "host"},
				{Service: "a", Field: "cgroup_parent", Value: "x"},
				{Service: "b", Field: "privileged", Value: "true"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tp := TargetPolicy{Repository: "https://github.com/o/r", Reference: "main"}
			if tc.tp != nil {
				tp = *tc.tp
			}

			f := &File{Version: 1, Default: tc.def, Targets: map[string]TargetPolicy{"noi/web": tp}}
			in := Input{
				Project: project(),
				Identity: Identity{
					Object: "noi/web", Repository: "https://github.com/o/r", Reference: "refs/heads/main",
				},
				Paths: Paths{CheckoutExternal: ext, CheckoutInternal: intl},
			}

			if tc.mod != nil {
				tc.mod(&in)
			}

			if got := f.Check(in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("violations = %#v\nwant         %#v", got, tc.want)
			}
		})
	}
}
