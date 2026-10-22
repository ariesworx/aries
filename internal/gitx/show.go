package gitx

import (
	"context"
	"fmt"
)

// ShowFile returns the file at path, slash-separated from the
// repository's top, as the commit rev has it, byte for byte, and whether
// rev has it; it fails when the repository has no commit rev.
func ShowFile(ctx context.Context, dir, rev, path string) ([]byte, bool, error) {
	if _, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}"); err != nil {
		return nil, false, fmt.Errorf("this repository has no commit %s", rev)
	}
	entry, err := gitRaw(ctx, dir, "ls-tree", "-z", rev, "--", path)
	if err != nil {
		return nil, false, err
	}
	if entry == "" {
		return nil, false, nil
	}
	out, err := gitRaw(ctx, dir, "cat-file", "blob", rev+":"+path)
	if err != nil {
		return nil, false, err
	}
	return []byte(out), true, nil
}
