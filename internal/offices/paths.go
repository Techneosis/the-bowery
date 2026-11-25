// Package offices is the office host's core: the four configuration files'
// shapes, the owner's state, placements, the fixed holdings checks, the rule
// contract and its evaluator, and the office log. The office host
// (cmd/office) and bowery's resolver (cmd/bowery) share it, so the two can
// never disagree about what a field means.
package offices

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// Layout is where one machine's offices keep their files. Root mode uses the
// system paths; dev mode (OFFICE_ROOT=<dir>) puts every office's config,
// state, inbox and log under one directory and runs as the invoking user.
// Everything else (rules, placements, queues, verdicts, logs) is the same
// code in both.
type Layout struct {
	Dev bool
	Etc string // region.toml, machine.toml, <office>/office.toml
	Var string // <office>/{state,log,accepted,...}, boxes/<box>, rules/<plugin>
}

func DetectLayout() Layout {
	if r := os.Getenv("OFFICE_ROOT"); r != "" {
		r, _ = filepath.Abs(r)
		return Layout{Dev: true, Etc: filepath.Join(r, "etc"), Var: filepath.Join(r, "var")}
	}
	return Layout{Etc: "/usr/local/etc/offices", Var: "/var/lib/offices"}
}

// DevNotice is the line every command prints in dev mode, naming each
// relaxation, so nobody mistakes a dev-mode office for a guarded one.
func (l Layout) DevNotice() string {
	if !l.Dev {
		return ""
	}
	return fmt.Sprintf("dev mode: OFFICE_ROOT=%s; not checking that config and plugins are root-owned, "+
		"that commands run as the office's user, or that an owner is the one opening or closing", filepath.Dir(l.Etc))
}

func (l Layout) RegionFile() string           { return filepath.Join(l.Etc, "region.toml") }
func (l Layout) MachineFile() string          { return filepath.Join(l.Etc, "machine.toml") }
func (l Layout) OfficeDir(o string) string    { return filepath.Join(l.Etc, o) }
func (l Layout) OfficeFile(o string) string   { return filepath.Join(l.Etc, o, "office.toml") }
func (l Layout) VarDir(o string) string       { return filepath.Join(l.Var, o) }
func (l Layout) StateFile(o string) string    { return filepath.Join(l.Var, o, "state") }
func (l Layout) LogFile(o string) string      { return filepath.Join(l.Var, o, "log") }
func (l Layout) Accepted(o string) string     { return filepath.Join(l.Var, o, "accepted") }
func (l Layout) Refused(o string) string      { return filepath.Join(l.Var, o, "refused") }
func (l Layout) Expired(o string) string      { return filepath.Join(l.Var, o, "expired") }
func (l Layout) VerdictCache(o string) string { return filepath.Join(l.Var, o, "verdicts.json") }
func (l Layout) BoxDir(box string) string     { return filepath.Join(l.Var, "boxes", box) }
func (l Layout) RuleState(p string) string    { return filepath.Join(l.Var, "rules", p) }

// ThisMachine is the short host name: what the machine file's `machine`
// must say.
func ThisMachine() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	if i := strings.IndexByte(h, '.'); i > 0 {
		h = h[:i]
	}
	return h
}

// Whoami is the invoking user's login name.
func Whoami() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}
