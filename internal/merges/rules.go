package merges

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/ariesworx/aries/internal/lazyregexp"
)

// Version is the version of the rule file this aries reads.
const Version = 1

// Rules is a rule file read: its classes in name order, and what it adds
// to the floor.
type Rules struct {
	Version int
	Classes []Class
	// Floor is the file's own floor: paths held whatever a class says,
	// beside the product's.
	Floor []string
	// SHA is the SHA-256 of the file's bytes, in hex, which a verdict
	// names so that it can be joined to the file it came from.
	SHA string
}

// Class is one class of pull request the rule lets be queued, with the
// conditions it checks. A condition left out is not checked, but checks
// is required of every class.
type Class struct {
	Name string
	// Checks is "green": every required check passed on the head commit.
	Checks string
	// Epic is "approved", or "": the issue sits under an epic whose
	// approval gate passed.
	Epic string
	// Opener, when set, asks who started the session that opened the pull
	// request. It has no trusted source yet and never passes.
	Opener *Opener
	// Allow, when set, are the paths every changed path must match, unless
	// it matches Except. No changed path may match Deny.
	Allow, Except, Deny []string
	// Tests is "no-deletions", or "": no test file loses a line or gains
	// a t.Skip.
	Tests string
}

// Opener is who may have started the session that opened a pull request.
type Opener struct {
	Harness   []string `yaml:"harness"`
	StartedBy []string `yaml:"started_by"`
}

type fileDoc struct {
	Version int                 `yaml:"version"`
	Floor   []string            `yaml:"floor"`
	Classes map[string]classDoc `yaml:"classes"`
	Holds   *string             `yaml:"holds"`
}

type classDoc struct {
	Checks string  `yaml:"checks"`
	Epic   string  `yaml:"epic"`
	Opener *Opener `yaml:"opener"`
	Paths  *struct {
		Allow  []string `yaml:"allow"`
		Except []string `yaml:"except"`
		Deny   []string `yaml:"deny"`
	} `yaml:"paths"`
	Tests string `yaml:"tests"`
}

var unknownField = lazyregexp.New(`line (\d+): field (\S+) not found in type`)

var className = lazyregexp.New(`^[a-z][a-z0-9-]{0,39}$`)

// Load reads a rule file. An unknown key, a value it does not know, a
// malformed pattern or a version this binary does not read is an error
// of one line saying where: a rule read loosely would queue what its
// author did not mean.
func Load(b []byte) (*Rules, error) {
	var f fileDoc
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil && !errors.Is(err, io.EOF) {
		return nil, yamlError(err)
	}
	switch {
	case f.Version == 0:
		return nil, errors.New("no version: the file begins `version: 1`")
	case f.Version != Version:
		return nil, fmt.Errorf("version %d: this aries reads version %d; upgrade aries", f.Version, Version)
	case len(f.Classes) == 0:
		return nil, errors.New("no classes: a rule with none queues nothing")
	case f.Holds != nil && *f.Holds != "draft":
		return nil, fmt.Errorf("holds: %q: the one hold is draft", *f.Holds)
	}
	sum := sha256.Sum256(b)
	r := &Rules{Version: f.Version, SHA: hex.EncodeToString(sum[:])}
	for i, p := range f.Floor {
		if err := checkPattern(p); err != nil {
			return nil, fmt.Errorf("floor %d: %q: %w", i+1, p, err)
		}
	}
	r.Floor = f.Floor
	names := make([]string, 0, len(f.Classes))
	for n := range f.Classes {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		c, err := loadClass(n, f.Classes[n])
		if err != nil {
			return nil, fmt.Errorf("class %s: %w", n, err)
		}
		r.Classes = append(r.Classes, c)
	}
	return r, nil
}

func loadClass(name string, d classDoc) (Class, error) {
	c := Class{Name: name, Checks: d.Checks, Epic: d.Epic, Opener: d.Opener, Tests: d.Tests}
	switch {
	case !className.MatchString(name):
		return c, errors.New("a class's name is lowercase letters, digits and hyphens, up to 40")
	case d.Checks == "":
		return c, errors.New("no checks: every class asks `checks: green`")
	case d.Checks != "green":
		return c, fmt.Errorf("checks: %q: the one value is green", d.Checks)
	case d.Epic != "" && d.Epic != "approved":
		return c, fmt.Errorf("epic: %q: the one value is approved", d.Epic)
	case d.Tests != "" && d.Tests != "no-deletions":
		return c, fmt.Errorf("tests: %q: the one value is no-deletions", d.Tests)
	case d.Opener != nil && len(d.Opener.Harness) == 0 && len(d.Opener.StartedBy) == 0:
		return c, errors.New("opener: name a harness or started_by")
	case d.Paths == nil:
		return c, errors.New("no paths: a class names the paths it allows or denies")
	}
	p := d.Paths
	if len(p.Allow) == 0 && len(p.Deny) == 0 {
		return c, errors.New("paths: name allow or deny")
	}
	if len(p.Except) > 0 && len(p.Allow) == 0 {
		return c, errors.New("paths: except needs allow")
	}
	for _, list := range []struct {
		name string
		pats []string
	}{{"allow", p.Allow}, {"except", p.Except}, {"deny", p.Deny}} {
		for i, pat := range list.pats {
			if err := checkPattern(pat); err != nil {
				return c, fmt.Errorf("paths %s %d: %q: %w", list.name, i+1, pat, err)
			}
		}
	}
	c.Allow, c.Except, c.Deny = p.Allow, p.Except, p.Deny
	return c, nil
}

// yamlError is the YAML module's error in one line, an unknown key named
// as such.
func yamlError(err error) error {
	msg := err.Error()
	if m := unknownField.FindStringSubmatch(msg); m != nil {
		return fmt.Errorf("line %s: unknown key %q", m[1], m[2])
	}
	msg = strings.TrimPrefix(msg, "yaml: ")
	return errors.New(strings.Join(strings.Fields(msg), " "))
}
