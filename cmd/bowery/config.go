package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is ~/.config/bowery/config.toml (BOWERY_CONFIG names another file).
// Every key is optional: a missing file is the defaults, and the defaults are
// what a single person on a single machine wants.
type Config struct {
	// The workspace dispatched agents get a tab in by default.
	Swarm string `toml:"swarm"`
	// The pane-metadata source the sound=off token is reported under. Whatever
	// beeps (a herdr plugin of your own) reads the token; the source only
	// says who set it.
	SoundSource string `toml:"sound_source"`
	// Folder-trust roots: Claude's "trust this folder?" dialog is answered Yes
	// under these (and always under the herdr worktrees dir). ~ is the home
	// directory of whichever machine the pane is on.
	TrustRoots []string `toml:"trust_roots"`
	// Short names for --model, expanded before they reach the agent. Anything
	// not listed is passed through as written (claude resolves its own aliases).
	Models map[string]string `toml:"models"`
	// Identities for agents on this machine's own server (a shared box
	// dispatching to itself): the same contract as a machine's
	// identity_command. Unset: --as is refused for a local dispatch.
	IdentityCommand string `toml:"identity_command"`
	IdentityEnv     string `toml:"identity_env"`
	// Per saved herdr machine (`herdr machine list`), by label.
	Machines map[string]MachineConfig `toml:"machines"`

	path   string
	loaded bool
}

type MachineConfig struct {
	// The command that runs things as a person on the box: `<cmd> <who>` is
	// their login shell, `<cmd> <who> <argv...>` runs argv as them, and both
	// export IdentityEnv=<who>. Unset: the machine has no identities, and
	// --as is refused for it. Written into a remote shell line unquoted, so
	// ~ works; a non-login ssh PATH is short, so give a full path.
	IdentityCommand string `toml:"identity_command"`
	// The variable the identity command sets to <who>; checked in the pane
	// before the agent starts.
	IdentityEnv string `toml:"identity_env"`
	// The ssh destination for git and file checks. Default: the herdr
	// profile's target.
	SSH string `toml:"ssh"`
	// Folder-trust roots on the box (~ is the box's home). Default: the
	// top-level trust_roots.
	TrustRoots []string `toml:"trust_roots"`
}

var cfg *Config

func configPath() string {
	if p := os.Getenv("BOWERY_CONFIG"); p != "" {
		return p
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(home(), ".config")
	}
	return filepath.Join(base, "bowery", "config.toml")
}

func loadConfig() *Config {
	c := &Config{path: configPath()}
	md, err := toml.DecodeFile(c.path, c)
	switch {
	case err == nil:
		c.loaded = true
		if un := md.Undecoded(); len(un) > 0 {
			keys := make([]string, len(un))
			for i, k := range un {
				keys[i] = k.String()
			}
			die("%s: unknown key(s) %s", c.path, strings.Join(keys, ", "))
		}
	case os.IsNotExist(err):
	default:
		die("%s: %v", c.path, err)
	}
	if c.Swarm == "" {
		c.Swarm = "swarm"
	}
	if c.SoundSource == "" {
		c.SoundSource = "bowery.sound"
	}
	if c.TrustRoots == nil {
		c.TrustRoots = []string{"~/dev"}
	}
	if c.IdentityEnv == "" {
		c.IdentityEnv = "BOWERY_AS"
	}
	for label, m := range c.Machines {
		if m.IdentityEnv == "" {
			m.IdentityEnv = "BOWERY_AS"
		}
		if m.TrustRoots == nil {
			m.TrustRoots = c.TrustRoots
		}
		c.Machines[label] = m
	}
	return c
}

func machineConfig(label string) MachineConfig {
	if label == "" {
		return MachineConfig{IdentityCommand: cfg.IdentityCommand, IdentityEnv: cfg.IdentityEnv, TrustRoots: cfg.TrustRoots}
	}
	if m, ok := cfg.Machines[label]; ok {
		return m
	}
	return MachineConfig{IdentityEnv: "BOWERY_AS", TrustRoots: cfg.TrustRoots}
}

// cmdConfig prints the settings in force and where they came from.
func cmdConfig(args []string) {
	if len(args) > 0 {
		die("config: unexpected %s", strings.Join(args, " "))
	}
	src := "defaults (no file)"
	if cfg.loaded {
		src = "read"
	}
	fmt.Printf("# %s: %s\n", cfg.path, src)
	fmt.Printf("swarm = %q\n", cfg.Swarm)
	fmt.Printf("sound_source = %q\n", cfg.SoundSource)
	fmt.Printf("trust_roots = %s\n", tomlList(cfg.TrustRoots))
	fmt.Printf("# plus the herdr worktrees dir: %s\n", localWorktreeRoot())
	fmt.Printf("# records: %s\n", stateDir())
	if cfg.IdentityCommand != "" {
		fmt.Printf("identity_command = %q\n", cfg.IdentityCommand)
		fmt.Printf("identity_env = %q\n", cfg.IdentityEnv)
	}
	if len(cfg.Models) > 0 {
		fmt.Println("\n[models]")
		for _, k := range sortedKeys(cfg.Models) {
			fmt.Printf("%s = %q\n", k, cfg.Models[k])
		}
	}
	labels := make([]string, 0, len(cfg.Machines))
	for l := range cfg.Machines {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	for _, l := range labels {
		m := cfg.Machines[l]
		fmt.Printf("\n[machines.%s]\n", l)
		if m.IdentityCommand != "" {
			fmt.Printf("identity_command = %q\n", m.IdentityCommand)
			fmt.Printf("identity_env = %q\n", m.IdentityEnv)
		} else {
			fmt.Println("# no identity_command: --as is refused here")
		}
		if m.SSH != "" {
			fmt.Printf("ssh = %q\n", m.SSH)
		}
		fmt.Printf("trust_roots = %s\n", tomlList(m.TrustRoots))
	}
}

func tomlList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = fmt.Sprintf("%q", x)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
