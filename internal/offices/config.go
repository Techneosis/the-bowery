package offices

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
)

// Names is a TOML value that may be one name or a list of names.
type Names []string

func (n *Names) UnmarshalTOML(v any) error {
	switch x := v.(type) {
	case string:
		*n = Names{x}
	case []any:
		out := make(Names, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return fmt.Errorf("a list of names holds a %T", e)
			}
			out = append(out, s)
		}
		*n = out
	default:
		return fmt.Errorf("want a name or a list of names, got %T", v)
	}
	return nil
}

// Instance is one rule instance (spec §5.1), wherever it appears.
type Instance struct {
	Name     string         `toml:"name" json:"name"`
	Plugin   string         `toml:"plugin" json:"plugin"`
	Contract int            `toml:"contract" json:"contract,omitempty"`
	At       []string       `toml:"at" json:"at,omitempty"`
	Timeout  string         `toml:"timeout" json:"timeout,omitempty"`
	Binding  bool           `toml:"binding" json:"binding,omitempty"`
	OnClosed string         `toml:"on_closed" json:"on_closed,omitempty"`
	Config   map[string]any `toml:"config" json:"config,omitempty"`

	source string // "region", "machine", "office"
}

// ---------------------------------------------------------------- region

type RegionOffice struct {
	Name    string `toml:"name"`
	Machine string `toml:"machine"`
}

type Tier struct {
	Effects []string `toml:"effects"`
	Holds   []string `toml:"holds"`
}

type Naming struct {
	UserMax int    `toml:"user_max"`
	Box     string `toml:"box"`
}

// RegionDefaults is `[defaults]`. The spec writes the rule defaults as
// `rules.default` beside `[[defaults.rules]]`, which TOML can't hold (one key
// can't be both a table and an array of tables), so here they are
// `rule_default`, `on_closed` and `interval`.
type RegionDefaults struct {
	Signers             string     `toml:"signers"`
	RuleDefault         string     `toml:"rule_default"`
	OnClosed            string     `toml:"on_closed"`
	Interval            string     `toml:"interval"`
	RuleTimeout         string     `toml:"rule_timeout"`
	RetryTick           string     `toml:"retry_tick"`
	SignedRequestWindow string     `toml:"signed_request_window"`
	AuditDigest         string     `toml:"audit_digest"`
	Rules               []Instance `toml:"rules"`
}

type Peer struct {
	Address  string     `toml:"address"`
	Key      string     `toml:"key"`
	MayPlace []string   `toml:"may_place"`
	MayView  []string   `toml:"may_view"`
	Rules    []Instance `toml:"rules"`
}

type Region struct {
	Region          string          `toml:"region"`
	Owner           Names           `toml:"owner"`
	Timezone        string          `toml:"timezone"`
	Key             string          `toml:"key"`
	Offices         []RegionOffice  `toml:"offices"`
	Priorities      []string        `toml:"priorities"`
	ReservedEffects []string        `toml:"reserved_effects"`
	MaxExpires      string          `toml:"max_expires"`
	Tiers           map[string]Tier `toml:"tiers"`
	Naming          Naming          `toml:"naming"`
	Defaults        RegionDefaults  `toml:"defaults"`
	Peers           map[string]Peer `toml:"peers"`
}

// ---------------------------------------------------------------- machine

type Plugin struct {
	Kind      string `toml:"kind"`
	Path      string `toml:"path"`
	SHA256    string `toml:"sha256"`
	Contracts []int  `toml:"contracts"`
	// state-file plugins (read, not yet evaluated: that kind is M2's)
	MaxAge          string            `toml:"max_age"`
	OpenField       string            `toml:"open_field"`
	ReasonField     string            `toml:"reason_field"`
	RetryAfterField string            `toml:"retry_after_field"`
	Facts           map[string]string `toml:"facts"`
}

type FactProvider struct {
	Path     string   `toml:"path"`
	SHA256   string   `toml:"sha256"`
	Interval string   `toml:"interval"`
	Facts    []string `toml:"facts"`
}

// Surface is how an accepted job is started in an office. The real surfaces
// are the transport spec's; until they land, a surface here is a command the
// pull job runs (as the office) with the job as JSON on stdin.
type Surface struct {
	Command []string `toml:"command"`
}

type Machine struct {
	Machine string `toml:"machine"`
	// Transport is the placement transport's name (default "dir").
	Transport string                  `toml:"transport"`
	Plugins   map[string]Plugin       `toml:"plugins"`
	Facts     map[string]FactProvider `toml:"facts"`
	Rules     []Instance              `toml:"rules"`
	Surfaces  map[string]Surface      `toml:"surfaces"`
}

// ---------------------------------------------------------------- office

type Holdings struct {
	Tools          []string `toml:"tools" json:"tools"`
	Repos          string   `toml:"repos" json:"-"`
	Secrets        []string `toml:"secrets" json:"secrets"`
	Qualifications []string `toml:"qualifications" json:"qualifications"`
	Effects        []string `toml:"effects" json:"effects"`
}

type OfficeRules struct {
	Default  string     `toml:"default"`
	OnClosed string     `toml:"on_closed"`
	Interval string     `toml:"interval"`
	Use      []Instance `toml:"use"`
}

type Office struct {
	Name     string      `toml:"name"`
	Region   string      `toml:"region"`
	Machine  string      `toml:"machine"`
	User     string      `toml:"user"`
	Owner    Names       `toml:"owner"`
	Tier     string      `toml:"tier"`
	ActsFor  string      `toml:"acts_for"`
	Kinds    []string    `toml:"kinds"`
	Profile  string      `toml:"profile"`
	Surfaces []string    `toml:"surfaces"`
	Signers  string      `toml:"signers"`
	Authors  string      `toml:"authors"`
	Jobs     string      `toml:"jobs"`
	Holdings Holdings    `toml:"holdings"`
	Rules    OfficeRules `toml:"rules"`

	// The lists the record names, read from beside office.toml.
	RepoList    []RepoHolding `toml:"-"`
	SignerNames []string      `toml:"-"`
	AuthorList  []string      `toml:"-"`
}

// RepoHolding is one line of the repos allow list:
// `owner/name modes=a,b [house=local] [scope=s]`.
type RepoHolding struct {
	Repo  string   `json:"repo"`
	Modes []string `json:"modes"`
	House string   `json:"house,omitempty"`
	Scope string   `json:"scope,omitempty"`
}

// NameRE is an office, plugin or instance name: a valid Unix user name.
var NameRE = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// Config is everything one office's evaluation needs.
type Config struct {
	L       Layout
	Region  *Region
	Machine *Machine
	Office  *Office
}

func decodeStrict(path string, v any) error {
	if err := checkRootOwned(path); err != nil {
		return err
	}
	md, err := toml.DecodeFile(path, v)
	if err != nil {
		return fmt.Errorf("%s: %v", path, err)
	}
	if un := md.Undecoded(); len(un) > 0 {
		keys := make([]string, 0, len(un))
		for _, k := range un {
			s := k.String()
			// A rule instance's config is the plugin's to judge, against its
			// own schema; everything else must be a key we know.
			if strings.Contains(s, ".config.") || strings.HasSuffix(s, ".config") {
				continue
			}
			keys = append(keys, s)
		}
		if len(keys) > 0 {
			return fmt.Errorf("%s: unknown key(s) %s", path, strings.Join(keys, ", "))
		}
	}
	return nil
}

// rootMode is set by LoadRegion and friends from the layout, so the
// ownership checks know whether to run.
var rootMode = true

// checkRootOwned: in root mode every config file and plugin is root's and
// writable by nobody else. Dev mode skips it (and says so).
func checkRootOwned(path string) error {
	if !rootMode {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot read its owner", path)
	}
	if st.Uid != 0 {
		return fmt.Errorf("%s: not owned by root", path)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s: writable by group or others", path)
	}
	return nil
}

func LoadRegion(l Layout) (*Region, error) {
	rootMode = !l.Dev
	r := &Region{}
	if err := decodeStrict(l.RegionFile(), r); err != nil {
		return nil, err
	}
	if r.Region == "" {
		return nil, fmt.Errorf("%s: no region name", l.RegionFile())
	}
	if r.Timezone == "" {
		r.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(r.Timezone); err != nil {
		return nil, fmt.Errorf("%s: timezone %q: %v", l.RegionFile(), r.Timezone, err)
	}
	if r.Naming.Box == "" {
		r.Naming.Box = "office-{name}"
	}
	if r.Naming.UserMax == 0 {
		r.Naming.UserMax = 32
	}
	for _, f := range []struct{ k, v string }{{"max_expires", r.MaxExpires}, {"defaults.rule_timeout", r.Defaults.RuleTimeout},
		{"defaults.retry_tick", r.Defaults.RetryTick}, {"defaults.interval", r.Defaults.Interval}} {
		if f.v != "" {
			if _, err := time.ParseDuration(f.v); err != nil {
				return nil, fmt.Errorf("%s: %s: %v", l.RegionFile(), f.k, err)
			}
		}
	}
	for i := range r.Defaults.Rules {
		r.Defaults.Rules[i].source = "region"
	}
	return r, nil
}

func LoadMachine(l Layout) (*Machine, error) {
	rootMode = !l.Dev
	m := &Machine{}
	if err := decodeStrict(l.MachineFile(), m); err != nil {
		return nil, err
	}
	if m.Machine == "" {
		return nil, fmt.Errorf("%s: no machine name", l.MachineFile())
	}
	for i := range m.Rules {
		m.Rules[i].source = "machine"
	}
	return m, nil
}

func LoadOffice(l Layout, name string) (*Office, error) {
	rootMode = !l.Dev
	if !NameRE.MatchString(name) {
		return nil, fmt.Errorf("%q is not an office name", name)
	}
	o := &Office{}
	if err := decodeStrict(l.OfficeFile(name), o); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no office %s on this machine (%s)", name, l.OfficeFile(name))
		}
		return nil, err
	}
	if o.Name != name {
		return nil, fmt.Errorf("%s: name is %q, not %q", l.OfficeFile(name), o.Name, name)
	}
	if o.User == "" {
		o.User = o.Name
	}
	if o.User != o.Name {
		return nil, fmt.Errorf("%s: user must equal name", l.OfficeFile(name))
	}
	switch o.Rules.Default {
	case "":
		o.Rules.Default = "closed"
	case "open", "closed":
	default:
		return nil, fmt.Errorf("%s: rules.default must be open or closed", l.OfficeFile(name))
	}
	for i := range o.Rules.Use {
		o.Rules.Use[i].source = "office"
	}
	dir := l.OfficeDir(name)
	var err error
	if o.Holdings.Repos != "" {
		if o.RepoList, err = readRepos(dir + "/" + o.Holdings.Repos); err != nil {
			return nil, err
		}
	}
	if o.Signers != "" {
		if o.SignerNames, err = readFirstFields(dir + "/" + o.Signers); err != nil {
			return nil, err
		}
	}
	if o.Authors != "" {
		if o.AuthorList, err = readLines(dir + "/" + o.Authors); err != nil {
			return nil, err
		}
	}
	return o, nil
}

// Load reads the region, the machine and one office, and checks the office
// is on this machine.
func Load(l Layout, office string) (*Config, error) {
	r, err := LoadRegion(l)
	if err != nil {
		return nil, err
	}
	m, err := LoadMachine(l)
	if err != nil {
		return nil, err
	}
	if here := ThisMachine(); m.Machine != here {
		return nil, fmt.Errorf("%s says machine %q, and this is %q: the office host runs only on its own machine", l.MachineFile(), m.Machine, here)
	}
	o, err := LoadOffice(l, office)
	if err != nil {
		return nil, err
	}
	if o.Machine != m.Machine {
		return nil, fmt.Errorf("office %s is for machine %q, not %q", o.Name, o.Machine, m.Machine)
	}
	if o.Region != r.Region {
		return nil, fmt.Errorf("office %s is in region %q, and this machine's region is %q", o.Name, o.Region, r.Region)
	}
	return &Config{L: l, Region: r, Machine: m, Office: o}, nil
}

// Box is the office's inbox name, from the region's naming template.
func (c *Config) Box() string { return BoxName(c.Region, c.Office.Name) }

func BoxName(r *Region, office string) string {
	return strings.ReplaceAll(r.Naming.Box, "{name}", office)
}

func readLines(path string) ([]string, error) {
	if err := checkRootOwned(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		out = append(out, s)
	}
	return out, sc.Err()
}

// readFirstFields reads an allowed-signers style file: the principal is the
// first field of each line.
func readFirstFields(path string) ([]string, error) {
	lines, err := readLines(path)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, strings.Fields(l)[0])
	}
	return out, nil
}

func readRepos(path string) ([]RepoHolding, error) {
	lines, err := readLines(path)
	if err != nil {
		return nil, err
	}
	var out []RepoHolding
	for _, l := range lines {
		f := strings.Fields(l)
		h := RepoHolding{Repo: f[0]}
		for _, kv := range f[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return nil, fmt.Errorf("%s: %q: want key=value", path, kv)
			}
			switch k {
			case "modes":
				h.Modes = strings.Split(v, ",")
			case "house":
				h.House = v
			case "scope":
				h.Scope = v
			default:
				return nil, fmt.Errorf("%s: unknown key %q", path, k)
			}
		}
		out = append(out, h)
	}
	return out, nil
}

// Duration parses a config duration, with a default for "".
func Duration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}
