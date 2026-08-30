// Package profiles implements named launch profiles: a profile carries only the
// engine fields it overrides and layers over config defaults (and under CLI
// flags). Files live at ~/.config/qfn/profiles/<name>.toml.
package profiles

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/BishopCodes/qfn-pgx/internal/config"
)

// Profile is an overlay over config.Engine. Every field is a pointer so
// "absent" and "set to the same value as the default" are distinguishable.
// CLI flags build a Profile on the fly (only Changed flags are set), which is
// also what makes precedence trivial: base ← profile ← flags.
type Profile struct {
	Name        string `toml:"-"`
	Description string `toml:"description,omitempty"`

	Mode         *string  `toml:"mode,omitempty"`
	PrefixCache  *bool    `toml:"prefix_cache,omitempty"`
	ExactTopK    *bool    `toml:"exact_topk,omitempty"`
	Ctx          *int     `toml:"ctx,omitempty"`
	Yarn         *bool    `toml:"yarn,omitempty"`
	MTP          *int     `toml:"mtp,omitempty"`
	Seqs         *int     `toml:"seqs,omitempty"`
	GpuMem       *float64 `toml:"gpu_mem,omitempty"`
	KVDtype      *string  `toml:"kv_dtype,omitempty"`
	Prewarm      *bool    `toml:"prewarm,omitempty"`
	Workers      *int     `toml:"workers,omitempty"`
	CPUSet       *string  `toml:"cpuset,omitempty"`
	Extra        *string  `toml:"extra,omitempty"`
	Port         *int     `toml:"port,omitempty"`
	Bind         *string  `toml:"bind,omitempty"`
	Lockdown     *bool    `toml:"lockdown,omitempty"`
	ContainerMem *string  `toml:"container_mem_cap,omitempty"`
	Image        *string  `toml:"image,omitempty"`
	Model        *string  `toml:"model,omitempty"`
	Name_        *string  `toml:"name,omitempty"` // docker container name (Name is the profile's own)
}

// Apply overlays p onto e (in place).
func (p *Profile) Apply(e *config.Engine) {
	set := func(dst *string, src *string) { if src != nil { *dst = *src } }
	seti := func(dst *int, src *int) { if src != nil { *dst = *src } }
	setb := func(dst *bool, src *bool) { if src != nil { *dst = *src } }
	setf := func(dst *float64, src *float64) { if src != nil { *dst = *src } }
	set(&e.Mode, p.Mode)
	setb(&e.PrefixCache, p.PrefixCache)
	setb(&e.ExactTopK, p.ExactTopK)
	seti(&e.Ctx, p.Ctx)
	setb(&e.Yarn, p.Yarn)
	seti(&e.MTP, p.MTP)
	seti(&e.Seqs, p.Seqs)
	setf(&e.GpuMem, p.GpuMem)
	set(&e.KVDtype, p.KVDtype)
	setb(&e.Prewarm, p.Prewarm)
	seti(&e.Workers, p.Workers)
	set(&e.CPUSet, p.CPUSet)
	set(&e.Extra, p.Extra)
	seti(&e.Port, p.Port)
	set(&e.Bind, p.Bind)
	setb(&e.Lockdown, p.Lockdown)
	set(&e.ContainerMem, p.ContainerMem)
	set(&e.Image, p.Image)
	set(&e.Model, p.Model)
	set(&e.Name, p.Name_)
}

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidateName rejects names that are not safe as filenames.
func ValidateName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("profile name %q must match [a-z0-9][a-z0-9._-]{0,63}", name)
	}
	return nil
}

// Dir is ~/.config/qfn/profiles.
func Dir() string { return filepath.Join(config.Dir(), "profiles") }

func path(name string) string { return filepath.Join(Dir(), name+".toml") }

// Load reads one profile.
func Load(name string) (*Profile, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path(name))
	if err != nil {
		return nil, err
	}
	var p Profile
	md, err := toml.Decode(string(b), &p)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path(name), err)
	}
	if und := md.Undecoded(); len(und) > 0 {
		var keys []string
		for _, k := range und {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("%s: unknown key(s): %s", path(name), strings.Join(keys, ", "))
	}
	p.Name = name
	return &p, nil
}

// Save writes one profile.
func Save(p *Profile) error {
	if err := ValidateName(p.Name); err != nil {
		return err
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	// Description is an omitempty field; the encoder writes the whole overlay.
	tbl := toml.NewEncoder(&b)
	if err := tbl.Encode(p); err != nil {
		return err
	}
	return os.WriteFile(path(p.Name), []byte(b.String()), 0o644)
}

// Delete removes one profile file.
func Delete(name string) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	return os.Remove(path(name))
}

// List returns profile names sorted.
func List() ([]string, error) {
	ents, err := os.ReadDir(Dir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() || filepath.Ext(e.Name()) != ".toml" {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".toml"))
	}
	sort.Strings(out)
	return out, nil
}

// FromEngine snapshots an (already resolved) engine config as a profile: what
// the wizard offers as "start from my current defaults".
func FromEngine(name, desc string, e config.Engine) *Profile {
	p := &Profile{Name: name, Description: desc}
	mode := e.Mode
	p.Mode = &mode
	pc := e.PrefixCache
	p.PrefixCache = &pc
	et := e.ExactTopK
	p.ExactTopK = &et
	ctx := e.Ctx
	p.Ctx = &ctx
	yn := e.Yarn
	p.Yarn = &yn
	mtp := e.MTP
	p.MTP = &mtp
	seqs := e.Seqs
	p.Seqs = &seqs
	gm := e.GpuMem
	p.GpuMem = &gm
	kv := e.KVDtype
	p.KVDtype = &kv
	pw := e.Prewarm
	p.Prewarm = &pw
	wk := e.Workers
	p.Workers = &wk
	cs := e.CPUSet
	p.CPUSet = &cs
	ex := e.Extra
	p.Extra = &ex
	port := e.Port
	p.Port = &port
	bd := e.Bind
	p.Bind = &bd
	lk := e.Lockdown
	p.Lockdown = &lk
	cm := e.ContainerMem
	p.ContainerMem = &cm
	im := e.Image
	p.Image = &im
	mdl := e.Model
	p.Model = &mdl
	nm := e.Name
	p.Name_ = &nm
	return p
}
