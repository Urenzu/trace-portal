package source

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Urenzu/trace-portal/internal/trace"
)

// maxWalkUp bounds the search for a repository root. Real trees are shallow;
// a deep walk would only ever be spent on paths that have no repository.
const maxWalkUp = 12

// projectResolver maps a working directory to the project it belongs to.
//
// The last path segment is not the project. Agents are routinely run from a
// subdirectory — fin-agentic/apps/web, grid-sim/app/src/components — and taking
// the basename turns each of those into its own "project", splitting one
// repository's cost across several rows and colliding unrelated directories
// that happen to share a name.
//
// Resolving to the enclosing repository root fixes both. Ingestion runs on the
// machine that produced the logs, so the directory is usually still there to
// look at; when it is not, the basename remains the only available answer.
type projectResolver struct {
	mu    sync.Mutex
	cache map[string]resolved
	// roots remembers repository roots already discovered, so a directory that
	// has since been deleted can still be matched by prefix against one of its
	// siblings that survived.
	roots []string
}

type resolved struct {
	name string
	id   string
	// repo records whether a working tree was actually found. A directory that
	// is not in one is still real work with a real cost, but it is not a
	// project, and presenting it as one is a mislabel.
	repo bool
}

func newProjectResolver() *projectResolver {
	return &projectResolver{cache: map[string]resolved{}}
}

// Resolve returns the project name, its stable id, and whether the directory
// belongs to a repository. The absolute path is never returned or stored.
func (r *projectResolver) Resolve(dir string) (name, id string, repo bool) {
	dir = strings.TrimRight(strings.TrimSpace(dir), `/\`)
	if dir == "" {
		return "", "", false
	}

	r.mu.Lock()
	if hit, ok := r.cache[dir]; ok {
		r.mu.Unlock()
		return hit.name, hit.id, hit.repo
	}
	r.mu.Unlock()

	root := r.repositoryRoot(dir)
	inRepo := root != ""
	if root == "" {
		// No repository to attribute this to — either the directory is not in
		// one, or it has since been deleted and cannot be checked. The
		// directory is then the honest answer. Two of them may share a name;
		// resolving that belongs to whatever displays them, not here, because
		// only the display knows which names actually appear together.
		root = dir
	}
	out := resolved{name: trace.Project(root), id: trace.ProjectID(root), repo: inRepo}

	r.mu.Lock()
	r.cache[dir] = out
	r.mu.Unlock()
	return out.name, out.id, out.repo
}

// repositoryRoot walks up from dir looking for a working tree, returning "" if
// there is none to find.
func (r *projectResolver) repositoryRoot(dir string) string {
	// A directory inside an already-known root belongs to it, whether or not it
	// still exists. This is what keeps a deleted subdirectory attributed to the
	// repository its siblings resolved to.
	if known := r.knownRoot(dir); known != "" {
		return known
	}

	current := dir
	for i := 0; i < maxWalkUp; i++ {
		if isRepositoryRoot(current) {
			r.remember(current)
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return ""
}

func (r *projectResolver) knownRoot(dir string) string {
	r.mu.Lock()
	defer r.mu.Unlock()

	best := ""
	for _, root := range r.roots {
		if underRoot(dir, root) && len(root) > len(best) {
			best = root
		}
	}
	return best
}

func (r *projectResolver) remember(root string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, existing := range r.roots {
		if existing == root {
			return
		}
	}
	r.roots = append(r.roots, root)
}

// isRepositoryRoot reports whether dir holds a git working tree. A .git entry
// may be a directory or, for worktrees and submodules, a file.
func isRepositoryRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// underRoot reports whether dir is root or sits beneath it, comparing case- and
// separator-insensitively so paths recorded from different shells still match.
func underRoot(dir, root string) bool {
	d := strings.ToLower(filepath.ToSlash(dir))
	rt := strings.ToLower(filepath.ToSlash(root))
	return d == rt || strings.HasPrefix(d, rt+"/")
}
