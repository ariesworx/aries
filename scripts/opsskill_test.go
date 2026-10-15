package scripts_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ariesworx/aries/internal/cli"
)

// The operations skill: a Claude Code plugin in this repository, listed
// by the marketplace file at the repository's root, that deploys and
// runs a server by the guides. These tests keep it true to the code and
// the guides: every command and flag it names exists, every section it
// cites is there, and its manifests and frontmatter are what Claude Code
// reads.
const (
	opsMarketplace = ".claude-plugin/marketplace.json"
	opsPlugin      = "plugins/starfix-ops"
	opsSkill       = opsPlugin + "/skills/starfix-ops"
)

// opsSkillFiles returns the skill's Markdown files, SKILL.md and each
// reference file, by path from the repository's root.
func opsSkillFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{opsSkill + "/SKILL.md": read(t, opsSkill+"/SKILL.md")}
	refs, err := filepath.Glob(filepath.Join("..", filepath.FromSlash(opsSkill), "references", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) == 0 {
		t.Fatalf("%s/references holds no reference file", opsSkill)
	}
	for _, r := range refs {
		p := opsSkill + "/references/" + filepath.Base(r)
		files[p] = read(t, p)
	}
	return files
}

// frontmatter returns the keys and values between the opening and the
// closing --- of a SKILL.md, each on one line, or fails the test.
func frontmatter(t *testing.T, path, text string) map[string]string {
	t.Helper()
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		t.Fatalf("%s does not start with a --- line: Claude Code reads frontmatter only from the first line", path)
	}
	block, _, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		t.Fatalf("%s's frontmatter has no closing --- line", path)
	}
	fields := map[string]string{}
	for i, line := range strings.Split(block, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || key == "" || strings.TrimSpace(key) != key {
			t.Fatalf("%s: frontmatter line %d is not KEY: VALUE on one line: %q", path, i+2, line)
		}
		if _, dup := fields[key]; dup {
			t.Errorf("%s: frontmatter key %q is given twice", path, key)
		}
		fields[key] = strings.TrimSpace(value)
	}
	return fields
}

// plainScalar reports why a YAML value written without quotes would not
// be read as the one string it looks like, or "" when it would be. A
// quoted value must close its quote. This is the subset of YAML the
// skill's frontmatter keeps to, not a YAML parser.
func plainScalar(v string) string {
	switch {
	case v == "":
		return "it is empty"
	case strings.HasPrefix(v, `"`):
		if len(v) < 2 || !strings.HasSuffix(v, `"`) || strings.Contains(v[1:len(v)-1], `"`) {
			return "its double quote is not closed at the end of the line, or it holds another"
		}
		return ""
	case strings.ContainsAny(v[:1], "[]{}&*!|>'%@`#,?:-"):
		return fmt.Sprintf("it starts with %q, which YAML reads as syntax", v[:1])
	case strings.Contains(v, ": ") || strings.Contains(v, " #"):
		return `it holds ": " or " #", which YAML reads as syntax; quote it`
	}
	return ""
}

// TestOpsSkillFrontmatter holds SKILL.md's frontmatter to the Agent
// Skills format, which Claude Code reads and which a skill must keep to
// for claude.ai and the API too: only the keys it allows, a name that is
// the directory's, and a description within its length. And SKILL.md
// stays under the 500 lines Claude Code's documentation advises; the
// detail goes in its reference files.
func TestOpsSkillFrontmatter(t *testing.T) {
	t.Parallel()
	path := opsSkill + "/SKILL.md"
	text := read(t, path)
	fields := frontmatter(t, path, text)
	allowed := []string{"name", "description", "license", "compatibility", "metadata", "allowed-tools"}
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		if !slices.Contains(allowed, k) {
			t.Errorf("%s: frontmatter key %q is not one the Agent Skills format allows (%s)", path, k, strings.Join(allowed, ", "))
		}
		if why := plainScalar(fields[k]); why != "" {
			t.Errorf("%s: frontmatter %s: %s", path, k, why)
		}
	}
	name := fields["name"]
	if !regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`).MatchString(name) || len(name) > 64 {
		t.Errorf("%s: name %q must be lowercase letters and digits with inner hyphens, at most 64 characters", path, name)
	}
	if dir := filepath.Base(opsSkill); name != dir {
		t.Errorf("%s: name %q is not its directory's name, %q", path, name, dir)
	}
	desc := strings.Trim(fields["description"], `"`)
	if desc == "" || len(desc) > 1024 || strings.ContainsAny(desc, "<>") {
		t.Errorf("%s: description must be 1 to 1024 characters with no < or >; it is %d", path, len(desc))
	}
	if n := strings.Count(text, "\n"); n >= 500 {
		t.Errorf("%s is %d lines; keep it under 500 and move detail to references/", path, n)
	}
}

// TestOpsSkillMap checks that SKILL.md links every reference file, so
// Claude knows when to load each, and that every link it makes to a file
// of the skill's resolves.
func TestOpsSkillMap(t *testing.T) {
	t.Parallel()
	files := opsSkillFiles(t)
	skill := files[opsSkill+"/SKILL.md"]
	linked := map[string]bool{}
	for _, m := range regexp.MustCompile(`\]\((references/[^)#\s]+)\)`).FindAllStringSubmatch(skill, -1) {
		linked[opsSkill+"/"+m[1]] = true
		if _, ok := files[opsSkill+"/"+m[1]]; !ok {
			t.Errorf("%s/SKILL.md links %s, which is not a reference file of the skill", opsSkill, m[1])
		}
	}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		if strings.Contains(p, "/references/") && !linked[p] {
			t.Errorf("%s/SKILL.md does not link %s: name it in the map of reference files", opsSkill, p)
		}
	}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		for _, m := range regexp.MustCompile(`\]\(([a-z0-9_-]+\.md)\)`).FindAllStringSubmatch(files[p], -1) {
			if _, ok := files[filepath.ToSlash(filepath.Join(filepath.Dir(p), m[1]))]; !ok {
				t.Errorf("%s links %s, which is not a file of the skill", p, m[1])
			}
		}
	}
}

// docAnchors returns the anchor GitHub gives each heading of a Markdown
// file, outside its code blocks, with -1, -2 … for a heading repeated.
func docAnchors(text string) map[string]bool {
	anchors := map[string]bool{}
	seen := map[string]int{}
	drop := regexp.MustCompile(`[^a-z0-9 _-]`)
	heading := regexp.MustCompile(`^#{1,6} (.+)$`)
	fenced := false
	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		m := heading.FindStringSubmatch(line)
		if fenced || m == nil {
			continue
		}
		a := strings.ReplaceAll(drop.ReplaceAllString(strings.ToLower(strings.ReplaceAll(m[1], "`", "")), ""), " ", "-")
		if n := seen[a]; n > 0 {
			anchors[fmt.Sprintf("%s-%d", a, n)] = true
		} else {
			anchors[a] = true
		}
		seen[a]++
	}
	return anchors
}

// citation finds a file of the repository the skill names, with the
// section of it when it names one: docs/server.md#upgrades. A path that
// follows a slash is part of a URL or of another path, and is not one.
var citation = regexp.MustCompile(`(?:^|[^/\w.-])((?:docs|deploy|scripts)/[A-Za-z0-9_./-]*[A-Za-z0-9_]|(?:RELEASING|CONTRIBUTING|README|AGENTS)\.md|install(?:-server)?\.sh)(#[A-Za-z0-9_-]+)?`)

// TestOpsSkillCitesGuides checks every file and section of the
// repository that the skill cites: the file exists, and a section is a
// heading of it. A guide's section that is renamed or removed then
// fails here, naming the reference file to change with it. Each
// reference file cites at least one section, since each step says which
// section of the guides it follows.
func TestOpsSkillCitesGuides(t *testing.T) {
	t.Parallel()
	files := opsSkillFiles(t)
	anchors := map[string]map[string]bool{}
	for _, p := range slices.Sorted(maps.Keys(files)) {
		sections := 0
		for _, m := range citation.FindAllStringSubmatch(files[p], -1) {
			target, anchor := m[1], strings.TrimPrefix(m[2], "#")
			if _, err := os.Stat(filepath.Join("..", filepath.FromSlash(target))); err != nil {
				t.Errorf("%s cites %s, which is not a file of the repository", p, target)
				continue
			}
			if anchor == "" {
				continue
			}
			if !strings.HasSuffix(target, ".md") {
				t.Errorf("%s cites %s#%s: only a Markdown file has sections", p, target, anchor)
				continue
			}
			if anchors[target] == nil {
				anchors[target] = docAnchors(read(t, target))
			}
			if !anchors[target][anchor] {
				t.Errorf("%s cites %s#%s, which is no heading of %s: follow the section where it went, and cite it", p, target, anchor, target)
			}
			sections++
		}
		if strings.Contains(p, "/references/") && sections == 0 {
			t.Errorf("%s cites no section of the guides (docs/FILE.md#section): say which section each step follows", p)
		}
	}
}

// usagePath is one form of a command in its usage of record: the words
// that name it, such as [admin key remove], and the flags it takes.
type usagePath struct {
	words []string
	flags map[string]bool
}

var (
	literalWord = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	usageFlag   = regexp.MustCompile(`(?:^|[\s\[|(])(--?[A-Za-z][A-Za-z0-9-]*)`)
)

// parseUsage reads usage text in the form sfx and starfixd print it: one
// form per line, or forms joined by " | ", where a form that does not
// start with its command's name continues the form before it, such as
// "admin key add NAME FILE | list NAME". prefix is the program's own
// name, which a line may begin with.
func parseUsage(text, prefix string, names []string) []usagePath {
	var paths []usagePath
	var group []string
	for line := range strings.SplitSeq(text, "\n") {
		for seg := range strings.SplitSeq(line, " | ") {
			fields := strings.Fields(seg)
			if len(fields) > 0 && fields[0] == prefix {
				fields = fields[1:]
			}
			if len(fields) == 0 {
				continue
			}
			var words []string
			for _, f := range fields {
				if !literalWord.MatchString(f) {
					break
				}
				words = append(words, f)
			}
			if len(words) == 0 {
				continue
			}
			if slices.Contains(names, words[0]) {
				group = words[:max(len(words)-1, 1)]
			} else if group != nil {
				words = append(slices.Clone(group), words...)
			} else {
				continue
			}
			flags := map[string]bool{}
			for _, m := range usageFlag.FindAllStringSubmatch(seg, -1) {
				flags[m[1]] = true
			}
			paths = append(paths, usagePath{words, flags})
		}
	}
	return paths
}

// sfxUsage returns the forms of every sfx command, from `sfx help` and
// `sfx help COMMAND`, which is how sfx states its own usage.
func sfxUsage(t *testing.T) []usagePath {
	t.Helper()
	help := func(args ...string) string {
		var out, errOut bytes.Buffer
		code := cli.Run(t.Context(), append([]string{"help"}, args...), cli.Env{
			Stdout: &out, Stderr: &errOut,
			Getenv: func(string) string { return "" },
			UserHomeDir: func() (string, error) {
				return "", fmt.Errorf("the test reads no home directory")
			},
		})
		if code != cli.ExitOK {
			t.Fatalf("sfx help %s = exit %d: %s", strings.Join(args, " "), code, errOut.String())
		}
		return out.String()
	}
	var names []string
	for _, m := range regexp.MustCompile(`(?m)^  ([a-z][a-z0-9-]*) `).FindAllStringSubmatch(help(), -1) {
		names = append(names, m[1])
	}
	if len(names) < 10 {
		t.Fatalf("sfx help lists %d commands: %v", len(names), names)
	}
	var paths []usagePath
	for _, n := range names {
		usage, _, _ := strings.Cut(strings.TrimPrefix(help(n), "usage: sfx "), "\n")
		paths = append(paths, parseUsage(usage, "sfx", []string{n})...)
	}
	return paths
}

// starfixdUsage returns the forms of every starfixd command, from the
// usage text starfixd prints (cmd/starfixd/main.go).
func starfixdUsage(t *testing.T) []usagePath {
	t.Helper()
	m := regexp.MustCompile("(?s)\nconst usage = `usage:\n(.*?)\n\n").FindStringSubmatch(read(t, "cmd/starfixd/main.go"))
	if m == nil {
		t.Fatal("cmd/starfixd/main.go has no const usage = `usage: …` block")
	}
	var names []string
	for _, l := range regexp.MustCompile(`(?m)^  starfixd ([a-z][a-z0-9-]*)`).FindAllStringSubmatch(m[1], -1) {
		names = append(names, l[1])
	}
	return parseUsage(m[1], "starfixd", names)
}

// invocation is a command line of sfx or starfixd that the skill gives.
type invocation struct {
	file, line string
	program    string
	words      []string
	flags      []string
}

// globalFlags are the flags a program takes before or after any of its
// commands, with whether each takes a value.
var globalFlags = map[string]map[string]bool{
	"sfx":      {"-C": true, "--json": false, "--no-queue": false, "-h": false, "--help": false},
	"starfixd": {"-h": false, "--help": false},
}

// notACommand are the words before which a program's name is looked up
// or matched, not run: command -v starfixd, pgrep starfixd.
var notACommand = []string{"which", "-v", "pgrep", "pkill"}

// skillCode returns the code of a Markdown text, line by line: each line
// of a fenced block of commands, with a line that ends in a backslash
// joined to the next, and each `span` of a paragraph, which may run
// across its lines. A block of another language (markdown, yaml) is not
// commands, and neither is text in "double quotes": that is how the
// skill quotes a message such as "starfixd is not running on the
// server".
func skillCode(text string) []string {
	var code []string
	span := regexp.MustCompile("`([^`]+)`")
	commands := []string{"", "sh", "bash", "console", "text"}
	fenced, skip, joined, para := false, false, "", ""
	flush := func() {
		for _, m := range span.FindAllStringSubmatch(para, -1) {
			code = append(code, m[1])
		}
		para = ""
	}
	for line := range strings.SplitSeq(text, "\n") {
		if lang, ok := strings.CutPrefix(strings.TrimSpace(line), "```"); ok {
			flush()
			fenced = !fenced
			skip = fenced && !slices.Contains(commands, lang)
			continue
		}
		if skip {
			continue
		}
		if fenced {
			if l, ok := strings.CutSuffix(line, `\`); ok {
				joined += l + " "
				continue
			}
			code = append(code, joined+line)
			joined = ""
			continue
		}
		switch {
		case strings.TrimSpace(line) == "":
			flush()
		case strings.HasPrefix(line, "|"):
			flush() // a table's row is a paragraph of its own
			para = line
			flush()
		default:
			para += " " + strings.TrimSpace(line)
		}
	}
	flush()
	return code
}

// skillInvocations returns every sfx and starfixd command line in the
// skill's code: the words that name the command, and its flags.
func skillInvocations(t *testing.T) []invocation {
	t.Helper()
	var invs []invocation
	files := opsSkillFiles(t)
	for _, p := range slices.Sorted(maps.Keys(files)) {
		for _, line := range skillCode(files[p]) {
			var toks []string
			for _, f := range strings.Fields(line) {
				toks = append(toks, strings.Trim(f, `'"()$`))
			}
			unit := false // in a systemctl or journalctl command, starfixd is a unit
			for i, tok := range toks {
				switch {
				case strings.HasSuffix(tok, "systemctl"), strings.HasSuffix(tok, "journalctl"):
					unit = true // also within $(…)
				case tok == "|", tok == "&&", tok == ";", tok == "||":
					unit = false
				}
				if unit {
					continue
				}
				program := tok[strings.LastIndex(tok, "/")+1:]
				if program != "sfx" && program != "starfixd" {
					continue
				}
				if strings.Contains(tok, "/") && !strings.HasPrefix(tok, "/") {
					continue // a relative path such as cmd/sfx is the source, not the program
				}
				if i > 0 && slices.Contains(notACommand, toks[i-1]) {
					continue
				}
				inv := invocation{file: p, line: line, program: program}
				rest := toks[i+1:]
				for len(rest) > 0 {
					if takes, ok := globalFlags[program][rest[0]]; ok && len(inv.words) == 0 {
						if takes && len(rest) > 1 {
							rest = rest[1:]
						}
						rest = rest[1:]
						continue
					}
					break
				}
				for _, r := range rest {
					if r == "|" || r == "&&" || r == ";" || r == "||" || r == ">" || r == "<" || r == "2>&1" || r == "--" {
						break
					}
					switch {
					case strings.HasPrefix(r, "-") && len(r) > 1:
						f, _, _ := strings.Cut(r, "=")
						inv.flags = append(inv.flags, f)
					case len(inv.flags) == 0 && literalWord.MatchString(r):
						inv.words = append(inv.words, r)
					}
				}
				if len(inv.words) == 0 && len(inv.flags) == 0 {
					continue // the program's name alone, in a sentence's code span
				}
				invs = append(invs, inv)
			}
		}
	}
	return invs
}

// TestOpsSkillCommandsExist checks every sfx and starfixd command the
// skill gives against the commands' own usage: the command, the words of
// its subcommand, and each flag it passes. A command or flag renamed or
// removed in the code then fails here, naming the skill's file.
func TestOpsSkillCommandsExist(t *testing.T) {
	t.Parallel()
	usage := map[string][]usagePath{"sfx": sfxUsage(t), "starfixd": starfixdUsage(t)}
	invs := skillInvocations(t)
	count := map[string]int{}
	for _, inv := range invs {
		count[inv.program]++
		// The form whose words are the longest prefix of the line's words:
		// the words after it are the command's arguments, such as a name.
		var best []usagePath
		for _, u := range usage[inv.program] {
			if len(u.words) > len(inv.words) || !slices.Equal(u.words, inv.words[:len(u.words)]) {
				continue
			}
			switch {
			case len(best) == 0 || len(u.words) > len(best[0].words):
				best = []usagePath{u}
			case len(u.words) == len(best[0].words):
				best = append(best, u)
			}
		}
		if len(best) == 0 && len(inv.flags) == 0 && slices.ContainsFunc(usage[inv.program], func(u usagePath) bool {
			return len(u.words) > len(inv.words) && slices.Equal(u.words[:len(inv.words)], inv.words)
		}) {
			continue // a group of commands named in a sentence, such as `sfx admin`
		}
		if len(best) == 0 {
			t.Errorf("%s: %s %s is no command of %s's usage: %q", inv.file, inv.program, strings.Join(inv.words, " "), inv.program, inv.line)
			continue
		}
		for _, f := range inv.flags {
			ok := false
			if _, global := globalFlags[inv.program][f]; global {
				ok = true
			}
			for _, u := range best {
				ok = ok || u.flags[f]
			}
			if !ok {
				t.Errorf("%s: %s %s takes no flag %s in its usage: %q", inv.file, inv.program, strings.Join(best[0].words, " "), f, inv.line)
			}
		}
	}
	for _, p := range []string{"sfx", "starfixd"} {
		if count[p] < 5 {
			t.Errorf("the skill gives %d %s commands; the test finds fewer than expected, so its reading of the skill is wrong", count[p], p)
		}
	}
}

// pluginName is a plugin's or a marketplace's name as Claude Code
// installs it: letters, digits, '.', '_' and '-', from a letter or digit.
var pluginName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// readJSON decodes a JSON file of the repository into v, refusing a
// document with trailing text.
func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(read(t, path)))
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	if dec.More() {
		t.Fatalf("%s: text after the JSON document", path)
	}
}

// TestOpsPluginManifests holds the marketplace file and the plugin's
// manifest to what Claude Code's plugin documentation requires: the
// marketplace's name, owner and plugins, each entry's name and a
// relative source inside the repository, and the manifest's name the
// entry's. The manifest sets no version: the plugin is versioned with
// starfix's releases, by the tag an operator adds the marketplace at,
// and a version string here would hold every operator on it.
func TestOpsPluginManifests(t *testing.T) {
	t.Parallel()
	var market struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Owner       struct {
			Name string `json:"name"`
		} `json:"owner"`
		Plugins []map[string]any `json:"plugins"`
	}
	readJSON(t, opsMarketplace, &market)
	reserved := regexp.MustCompile(`(?i)claude|anthropic`)
	if !pluginName.MatchString(market.Name) || strings.Contains(market.Name, "..") || reserved.MatchString(market.Name) {
		t.Errorf("%s: name %q must be letters, digits, '.', '_' and '-', from a letter or digit, and name no Anthropic product", opsMarketplace, market.Name)
	}
	if market.Owner.Name == "" || market.Description == "" {
		t.Errorf("%s: owner.name and description are required", opsMarketplace)
	}
	if len(market.Plugins) == 0 {
		t.Fatalf("%s lists no plugin", opsMarketplace)
	}
	sources := map[string]string{}
	for i, p := range market.Plugins {
		name, _ := p["name"].(string)
		source, _ := p["source"].(string)
		if !pluginName.MatchString(name) || reserved.MatchString(name) {
			t.Errorf("%s: plugins[%d].name %q is not a plugin name Claude Code installs", opsMarketplace, i, name)
		}
		if !strings.HasPrefix(source, "./") || strings.Contains(source, "..") || strings.Contains(source, `\`) {
			t.Errorf("%s: plugins[%d].source %q must be a path from the repository's root, starting ./, with no .. or backslash", opsMarketplace, i, source)
			continue
		}
		if _, ok := p["version"]; ok {
			t.Errorf("%s: plugins[%d] sets a version; the plugin is versioned by the release tag", opsMarketplace, i)
		}
		sources[name] = strings.TrimPrefix(source, "./")
	}
	if sources["starfix-ops"] != opsPlugin {
		t.Fatalf("%s lists starfix-ops at %q, not ./%s", opsMarketplace, sources["starfix-ops"], opsPlugin)
	}

	manifest := opsPlugin + "/.claude-plugin/plugin.json"
	var plugin map[string]any
	readJSON(t, manifest, &plugin)
	known := []string{"$schema", "name", "displayName", "description", "author", "homepage", "repository", "license", "keywords"}
	for _, k := range slices.Sorted(maps.Keys(plugin)) {
		switch {
		case k == "version":
			t.Errorf("%s sets a version: the plugin is versioned by the release tag an operator adds the marketplace at, and a version here would hold every operator on it", manifest)
		case !slices.Contains(known, k):
			t.Errorf("%s: field %q is not one this plugin uses (%s); a component field would move its skill from skills/", manifest, k, strings.Join(known, ", "))
		}
	}
	if name, _ := plugin["name"].(string); name != "starfix-ops" {
		t.Errorf("%s: name %q is not the marketplace entry's, starfix-ops", manifest, name)
	}
	if d, _ := plugin["description"].(string); d == "" {
		t.Errorf("%s: description is empty", manifest)
	}
	if a, _ := plugin["author"].(map[string]any); a == nil || a["name"] == "" || a["name"] == nil {
		t.Errorf("%s: author.name is required", manifest)
	}
	if l, _ := plugin["license"].(string); l != "Apache-2.0" {
		t.Errorf("%s: license %q is not the repository's, Apache-2.0", manifest, l)
	}
	if h, ok := plugin["homepage"].(string); ok {
		if u, err := url.Parse(h); err != nil || u.Scheme != "https" || u.Host == "" {
			t.Errorf("%s: homepage %q must be an https URL, or Claude Code does not load the plugin", manifest, h)
		}
	}
	for _, unwanted := range []string{"CLAUDE.md", "bin"} {
		if _, err := os.Stat(filepath.Join("..", filepath.FromSlash(opsPlugin), unwanted)); err == nil {
			t.Errorf("%s/%s: Claude Code does not load a plugin's CLAUDE.md, and claude.ai does not install a plugin with bin/", opsPlugin, unwanted)
		}
	}
}
