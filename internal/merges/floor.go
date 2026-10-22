package merges

// Area is a part of the floor: what it is, and the paths that hold it.
type Area struct {
	Role  string
	Paths []string
}

// File is where the rule lives in a repository, read from the default
// branch.
const File = ".aries/merges.yaml"

// Floor is the product's floor (docs/design/merges.md §5, decision 4):
// the paths no class lifts, held for every project whatever its rule
// says. A rule may add to it (its floor: list) and never shorten it.
var Floor = []Area{
	{"releases and signing", []string{
		"internal/release/**",
		"internal/tools/releasekey/**",
		"internal/tools/releaseindex/**",
		"RELEASING.md",
		"CHANGELOG.md",
		"docs/releases/**",
		"install*.sh",
	}},
	{"CI workflows", []string{
		".github/workflows/**",
		".github/actions/**",
		".github/CODEOWNERS",
	}},
	{"dependency files", []string{
		"**/go.mod",
		"**/go.sum",
		"deps.allow",
		".github/dependabot.yml",
	}},
	{"what agents may run", []string{
		".aries/guard.yaml",
		".aries/brief.txt",
		".claude/settings*.json",
		".codex/config.toml",
		".gemini/settings.json",
	}},
	{"the merge rule", []string{File}},
}
