package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// errNoPool means the directory we asked about is not inside a git repo, so
// there is no pool to report. treehouse scopes its pool to a repository, not
// to the machine, which is why Status takes a directory at all.
var errNoPool = errors.New("not a repository")

type treehouseClient struct{ r runner }

// thWorktree mirrors the object treehouse prints for `status --json`. Field
// names were taken from the binary's own struct tags; unknown keys decode away
// harmlessly, so a treehouse that grows a field will not break us.
type thWorktree struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Status      string `json:"status"`
	Branch      string `json:"branch"`
	LeaseID     string `json:"lease_id"`
	LeaseHolder string `json:"lease_holder"`
}

// Status reports the worktree pool for the repo containing dir. An empty pool
// is not an error: most repos do not have one.
func (t treehouseClient) Status(ctx context.Context, dir string) ([]Worktree, error) {
	if dir == "" {
		return nil, errNoPool
	}
	out, err := t.r.run(ctx, dir, "treehouse", "status", "--json")
	if err != nil {
		if strings.Contains(err.Error(), "not in a git or jj repository") {
			return nil, errNoPool
		}
		return nil, err
	}
	var raw []thWorktree
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("treehouse: parsing status: %w", err)
	}
	worktrees := make([]Worktree, 0, len(raw))
	for _, w := range raw {
		worktrees = append(worktrees, Worktree{
			Name:    w.Name,
			Path:    w.Path,
			Branch:  w.Branch,
			Status:  w.Status,
			Leased:  w.LeaseID != "" || !strings.EqualFold(w.Status, "free"),
			Holder:  w.LeaseHolder,
			LeaseID: w.LeaseID,
		})
	}
	return worktrees, nil
}
