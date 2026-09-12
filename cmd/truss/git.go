// git.go drives the git binary that backs root discovery and checkout.
// docs/port-plan.md §4.4 specifies a Git type living in internal/repo, but
// the package as actually built (internal/repo/roots.go) contains only the
// pure TouchedRoots function -- no Git struct exists anywhere in the tree.
// Rather than guess at, or silently add, an exported API to a package the
// task described as already built and pushed, the git driver lives here in
// cmd/truss instead: it is only ever used by the apply pass, behind the
// gitDriver interface below so tests can fake it. See the final report for
// this as a named deviation.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/beeradb/truss/internal/childproc"
	"github.com/beeradb/truss/internal/repo"
	"strings"
	"unicode"
)

// gitDriver is the subset of git operations the apply pass needs, matching
// docs/port-plan.md §4.4's Git type shape. A local interface rather than a
// concrete dependency on execGit so tests exercise the pass without a real
// git binary or network.
type gitDriver interface {
	// WithToken returns a driver that attaches the installation token to
	// EVERY git call it makes, not just the two that obviously touch the
	// network.
	//
	// ⚠️ THE TOKEN USED TO BE A PER-CALL PARAMETER ON CLONE AND FETCH ONLY,
	// AND IT NEVER REACHED THE OPERATIONS THAT NEEDED IT. The clone is
	// --filter=blob:none, so git lazily fetches blobs later: `diff
	// --name-only` does it for rename detection and `checkout` does it to
	// materialise a working tree. Both ran unauthenticated, so on a private
	// repo they 401 -- and the error names checkout or diff, not auth.
	// apply.sh got away with it because the token lived in
	// remote.origin.url. Moving it out of the URL was right; it just was not
	// carried to the rest. Found by the 2026-09-08 code audit.
	//
	// It returns a copy rather than mutating, so the token is still never
	// stored anywhere longer-lived than one pass, and it still travels only
	// in the child's environment -- never argv, never a URL, and never
	// `git config`-ed into the clone, which would restore the on-disk leak
	// the port removed.
	WithToken(token string) gitDriver

	EnsureClone(ctx context.Context, repoURL string) error
	Fetch(ctx context.Context, remote, branch string) error
	Commits(ctx context.Context, from, to string) ([]string, error)
	ChangedFiles(ctx context.Context, sha string) ([]string, error)
	TreeRoots(ctx context.Context, sha string) ([]string, error)

	// TreeRenderUnits lists every Kustomize unit present in a commit's tree.
	// It is separate from TreeRoots rather than folded into it because
	// TreeRoots reproduces the bash's `ls-tree -d ... -- platform projects/`
	// exactly and internal/parity compares against recordings of that; a
	// widened listing there would change what the pass plans for commits the
	// corpus already has answers for.
	TreeRenderUnits(ctx context.Context, sha string) ([]string, error)

	// TreeTofuUnits lists every credentials/tofu unit present in a commit's
	// tree: "credentials", "platform", and every clusters/<name>,
	// hosts/<name> and projects/<name> directory. It is separate from
	// TreeRoots for the same reason TreeRenderUnits is: TreeRoots reproduces
	// the bash's exact listing and internal/parity compares it against
	// recordings of that; this is the tree half of repo.TouchedUnits, which
	// runCommitLoop now derives tofu work from instead of repo.TouchedRoots.
	TreeTofuUnits(ctx context.Context, sha string) ([]string, error)

	// TreeAnsibleUnits lists every Ansible play present in a commit's tree:
	// every ansible/plays/<name> directory. Separate from the other two for
	// the same reason they are separate from TreeRoots -- TreeRoots
	// reproduces the bash's exact listing and internal/parity compares it
	// against recordings of that -- and separate from TreeTofuUnits because
	// the two feed different halves of one repo.TouchedUnits call, so
	// neither half can ever contain a unit of the other's kind.
	TreeAnsibleUnits(ctx context.Context, sha string) ([]string, error)

	Checkout(ctx context.Context, ref string) error
	HasDir(root string) bool

	// PushRef fast-forwards a remote ref to sha. It is the only write this
	// driver performs, and the only thing truss publishes anywhere.
	PushRef(ctx context.Context, sha, ref string) error

	// TreeFS returns a read-only fs.FS over sha's own tree, so
	// inventory.Load can read a commit's inventory WITHOUT checking it out
	// -- see execGit.TreeFS's own doc for why the apply pass needs exactly
	// that.
	TreeFS(ctx context.Context, sha string) (fs.FS, error)

	// Parent returns sha's first parent, and false when sha has none -- a
	// root commit -- or the parent could not be determined. The commit-loop
	// inventory gate uses this to fetch the PARENT's tree via TreeFS and
	// compare it against the head's, so a stateful workload's move between
	// clusters is caught (inventory.CheckMoves needs both snapshots at
	// once). false is deliberately not an error: a root commit is not a
	// failure to be propagated, it is the one case CheckMoves has nothing to
	// compare against.
	Parent(ctx context.Context, sha string) (string, bool)
}

// execGit drives the real git binary. The installation token is passed in
// per call, as a parameter, rather than held as state -- the apply pass
// mints it lazily (§2 item 12) and this keeps execGit itself stateless. It
// is NEVER placed in argv or in a URL passed as an argument (§3.5, §2's "no
// credential ever reaches an error string" spirit extended to argv, which
// `ps` on the same box can read as easily as a log): it reaches git only
// via GIT_CONFIG_KEY_0/GIT_CONFIG_VALUE_0 in the child's environment,
// git's own mechanism (since 2.31) for setting config without a file or a
// command-line flag.
type execGit struct {
	Bin, Dir string
	Stderr   io.Writer

	// token is attached to every call this driver makes. Unexported and set
	// only through WithToken, so there is one way for it to arrive.
	token string
}

func (g execGit) WithToken(token string) gitDriver {
	g.token = token // g is a copy: the receiver is by value
	return g
}

var treeRootPattern = regexp.MustCompile(`^(platform|projects/[^/]+)$`)

// checkRef refuses a ref that could mean something to git other than "a
// commit".
//
// ⚠️ THE REF COMES FROM THE LEDGER, WHICH IS A BUCKET. applied/HEAD is read
// and handed to `git checkout` and to `rev-list <last>..origin/main`, so
// anyone who can write that object chooses an argument to git -- and git's
// option surface is large. §4.4 specified this guard and it was never
// implemented; raised by the 2026-09-08 security review.
//
// ⚠️ IT REJECTS WHAT IS DANGEROUS, NOT WHAT IS UNFAMILIAR, AND §4.4'S LITERAL
// RULE WAS TRIED FIRST. That rule -- "a full hex sha or origin/<branch>" --
// is true of every ref in production and it BROKE internal/parity, whose
// recorded corpus carries the bash suite's own synthetic refs ("sha1",
// "base"). Breaking the acceptance test to satisfy a sentence in the spec is
// the wrong trade: parity is the evidence, the sentence is a description of
// it. A ref that is merely not a sha cannot do harm -- git fails to resolve
// it, which is a refusal. A ref that begins with "-" is an OPTION, and one
// carrying whitespace or a control character is smuggling a second argument.
// Those are the two things worth refusing, so those are what this refuses.
func checkRef(ref string) error {
	if ref == "" {
		return errors.New("refusing to pass an empty ref to git")
	}
	if strings.HasPrefix(ref, "-") {
		return fmt.Errorf("refusing to pass %q to git: a ref beginning with - is an option, not a commit", ref)
	}
	for _, r := range ref {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("refusing to pass %q to git: a ref carrying whitespace or a control character is smuggling a second argument", ref)
		}
	}
	return nil
}

func (g execGit) EnsureClone(ctx context.Context, repoURL string) error {
	if g.hasGitDir() {
		return nil
	}
	args := []string{"clone", "--filter=blob:none", repoURL, g.Dir}
	// clone has no repo to run -C into yet, so it runs from "" (the
	// current directory) with an explicit target.
	_, err := g.run(ctx, "", args)
	return err
}

func (g execGit) Fetch(ctx context.Context, remote, branch string) error {
	_, err := g.run(ctx, g.Dir, []string{"-C", g.Dir, "fetch", remote, branch})
	return err
}

func (g execGit) Commits(ctx context.Context, from, to string) ([]string, error) {
	if err := checkRef(from); err != nil {
		return nil, err
	}
	if err := checkRef(to); err != nil {
		return nil, err
	}
	out, err := g.run(ctx, g.Dir, []string{"-C", g.Dir, "rev-list", "--reverse", "--first-parent", from + ".." + to})
	if err != nil {
		return nil, fmt.Errorf("git rev-list %s..%s: %w", from, to, err)
	}
	return splitLines(out), nil
}

func (g execGit) ChangedFiles(ctx context.Context, sha string) ([]string, error) {
	if err := checkRef(sha); err != nil {
		return nil, err
	}
	out, err := g.run(ctx, g.Dir, []string{"-C", g.Dir, "diff", "--name-only", sha + "^", sha})
	if err != nil {
		return nil, fmt.Errorf("git diff --name-only %s^ %s: %w", sha, sha, err)
	}
	return splitLines(out), nil
}

// TreeRoots lists the "platform" and "projects/<name>" directories present
// in sha's own tree, reproducing derive_touched_roots' shared-input branch
// (`git ls-tree -d --name-only <sha> -- platform projects/ | grep -E
// '^(platform|projects/[^/]+)$'`), filter included.
// TreeRenderUnits lists the render units in a commit's tree: every
// baselines/<name> and deliveries/<cluster>/<unit> directory.
//
// ⚠️ IT IS -r AND NOT PLAIN -d, BECAUSE THE TWO KINDS SIT AT DIFFERENT
// DEPTHS. A baseline is one level under its prefix and a delivery is two,
// so a single non-recursive listing can reach one or the other but never
// both. Recursing and then asking repo.KindOf about each line is what keeps
// the depths in one place -- the same function the commit diff is matched
// against, so a listing and a diff can never disagree about what a directory
// is.
// PushRef fast-forwards refs/heads/<ref> on the remote to sha.
//
// ⚠️ NO --force AND NO LEASE, DELIBERATELY: THE ORDERING PROPERTY IS GIT'S,
// NOT OURS. A plain push refuses a non-fast-forward on its own, so a ref that
// somebody has moved elsewhere makes this fail loudly rather than overwrite
// whatever they did. Adding --force-with-lease would be the reflex and would
// be wrong -- it would make truss the thing that resolves the disagreement,
// when a delivery ref disagreeing with the applier is a fact somebody needs
// to look at.
//
// The refspec names the destination in full so a local branch of the same
// name can never be what is pushed.
func (g execGit) PushRef(ctx context.Context, sha, ref string) error {
	if err := checkRef(sha); err != nil {
		return err
	}
	if err := checkRef(ref); err != nil {
		return err
	}
	spec := fmt.Sprintf("%s:refs/heads/%s", sha, ref)
	if _, err := g.run(ctx, g.Dir, []string{"-C", g.Dir, "push", "origin", spec}); err != nil {
		return fmt.Errorf("git push origin %s: %w", spec, err)
	}
	return nil
}

func (g execGit) TreeRenderUnits(ctx context.Context, sha string) ([]string, error) {
	if err := checkRef(sha); err != nil {
		return nil, err
	}
	out, err := g.run(ctx, g.Dir, []string{"-C", g.Dir, "ls-tree", "-d", "-r", "--name-only", sha, "--", "baselines", "deliveries"})
	if err != nil {
		return nil, fmt.Errorf("git ls-tree -d -r %s: %w", sha, err)
	}
	var units []string
	for _, line := range splitLines(out) {
		if kind, ok := repo.KindOf(line); ok && kind == repo.KindRender {
			units = append(units, line)
		}
	}
	return units, nil
}

// TreeTofuUnits lists the tofu units present in sha's own tree: every
// clusters/<name>, hosts/<name>, platform and projects/<name> directory.
//
// ⚠️ "credentials" IS DELIBERATELY NOT LISTED HERE, THE SAME AS TreeRoots.
// Both repo.TouchedRoots and repo.TouchedUnits add "credentials" only when a
// changed file matches credentials/ -- never from its presence in the tree
// -- so listing it here would replan it on every shared-input commit
// (modules/, providers.allow, .opentofu-version) that TouchedRoots would
// not, breaking the equivalence internal/parity and TestTouchedUnitsTofu-
// HalfMatchesTouchedRoots (internal/repo) both hold it to.
//
// ⚠️ IT IS -r, THE SAME REASON TreeRenderUnits IS -- NOT BECAUSE THE
// EXISTING TWO PREFIXES NEEDED IT (plain -d already lists "platform" and
// "projects/<name>" correctly, which is why TreeRoots gets away without it),
// BUT BECAUSE "platform" MATCHES ITSELF DIRECTLY WHILE THE OTHER THREE
// PREFIXES NEED ONE LEVEL OF CHILDREN, and a future unit kind nested one
// level deeper would be silently dropped by a non-recursive listing rather
// than caught. Recursing and asking repo.KindOf about each line keeps the
// depth question in one place -- the same function the commit diff is
// matched against -- rather than encoded twice, once in a pathspec and once
// in KindOf's regexes, where they could disagree.
func (g execGit) TreeTofuUnits(ctx context.Context, sha string) ([]string, error) {
	if err := checkRef(sha); err != nil {
		return nil, err
	}
	out, err := g.run(ctx, g.Dir, []string{"-C", g.Dir, "ls-tree", "-d", "-r", "--name-only", sha, "--", "clusters", "hosts", "platform", "projects"})
	if err != nil {
		return nil, fmt.Errorf("git ls-tree -d -r %s: %w", sha, err)
	}
	var units []string
	for _, line := range splitLines(out) {
		if kind, ok := repo.KindOf(line); ok && (kind == repo.KindCredentials || kind == repo.KindTofu) {
			units = append(units, line)
		}
	}
	return units, nil
}

// TreeAnsibleUnits lists the plays present in sha's own tree: every
// ansible/plays/<name> directory.
//
// ⚠️ IT IS -r FOR THE REASON TreeTofuUnits RECORDS, AND HERE IT IS NOT
// OPTIONAL: "ansible/plays" is two levels above the unit, so a non-recursive
// listing would return the single line "ansible/plays" -- which repo.KindOf
// rejects, because a play is ansible/plays/<name>/ and not the directory
// holding them. The result would be an empty set for every commit, and an
// empty set here reads as "this commit configures no machine", which is the
// silent-noop shape this listing exists to close.
func (g execGit) TreeAnsibleUnits(ctx context.Context, sha string) ([]string, error) {
	if err := checkRef(sha); err != nil {
		return nil, err
	}
	out, err := g.run(ctx, g.Dir, []string{"-C", g.Dir, "ls-tree", "-d", "-r", "--name-only", sha, "--", "ansible/plays"})
	if err != nil {
		return nil, fmt.Errorf("git ls-tree -d -r %s: %w", sha, err)
	}
	var units []string
	for _, line := range splitLines(out) {
		if kind, ok := repo.KindOf(line); ok && kind == repo.KindAnsible {
			units = append(units, line)
		}
	}
	return units, nil
}

func (g execGit) TreeRoots(ctx context.Context, sha string) ([]string, error) {
	if err := checkRef(sha); err != nil {
		return nil, err
	}
	out, err := g.run(ctx, g.Dir, []string{"-C", g.Dir, "ls-tree", "-d", "--name-only", sha, "--", "platform", "projects/"})
	if err != nil {
		return nil, fmt.Errorf("git ls-tree -d %s: %w", sha, err)
	}
	var roots []string
	for _, line := range splitLines(out) {
		if treeRootPattern.MatchString(line) {
			roots = append(roots, line)
		}
	}
	return roots, nil
}

func (g execGit) Checkout(ctx context.Context, ref string) error {
	if err := checkRef(ref); err != nil {
		return err
	}
	_, err := g.run(ctx, g.Dir, []string{"-C", g.Dir, "checkout", "--quiet", ref})
	if err != nil {
		return fmt.Errorf("git checkout %s: %w", ref, err)
	}
	return nil
}

func (g execGit) HasDir(root string) bool {
	info, err := os.Stat(filepath.Join(g.Dir, root))
	return err == nil && info.IsDir()
}

func (g execGit) hasGitDir() bool {
	info, err := os.Stat(filepath.Join(g.Dir, ".git"))
	return err == nil && info.IsDir()
}

// run executes git with args, sending combined output to g.Stderr (never
// this process's real stdout -- §2 item 17's discipline applies here too:
// nothing about a git call belongs on a channel a caller might parse as
// JSON). A non-empty token attaches the installation token via env, for
// the two operations (clone, fetch) that touch the network.
func (g execGit) run(ctx context.Context, dir string, args []string) (string, error) {
	bin := g.Bin
	if bin == "" {
		bin = "git"
	}
	cmd := childproc.Command(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// ⚠️ AN EXPLICIT ENVIRONMENT, NOT os.Environ(). It used to inherit the
	// pod's whole environment, which contradicts the discipline plan.Runner
	// already enforces for tofu -- and is not merely untidy here: GIT_TRACE
	// or GIT_CURL_VERBOSE present in the pod would make git print the
	// Authorization header this function is careful to keep out of argv and
	// off disk, straight into the pod log. Raised by the 2026-09-08 security
	// review. PATH so git can find its own helper programs, HOME because git
	// reads ~/.gitconfig and an unset HOME makes it complain.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	if g.token != "" {
		cmd.Env = append(cmd.Env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraheader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+basicAuth(g.token),
		)
	}

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	runErr := cmd.Run()
	out := buf.String()
	if g.Stderr != nil {
		_, _ = g.Stderr.Write(buf.Bytes())
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return out, fmt.Errorf("exit %d", exitErr.ExitCode())
		}
		return out, runErr
	}
	return out, nil
}

// basicAuth builds the same "x-access-token:<token>" Basic credential the
// bash embedded in the clone URL (apply.sh:373); here it travels as a
// header value in the child's environment, never in argv or in the URL.
func basicAuth(token string) string {
	return base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// treeFS is a read-only fs.FS over one commit's tree, backed by git itself
// rather than by a checkout.
//
// ⚠️ IT EXISTS BECAUSE THE PASS NEEDS TWO COMMITS AT ONCE. Detecting that a
// workload MOVED means comparing a commit's inventory against its parent's,
// and the pass has the head checked out. Checking the parent out to read it
// would trample the tree it is about to apply from, so the parent is read
// through git plumbing instead and nothing on disk moves.
//
// Only the prefixes the inventory actually needs are listed, because a
// recursive listing of a whole platform repository costs more than the
// question is worth and the answer would be discarded anyway.
type treeFS struct {
	g    execGit
	ctx  context.Context
	sha  string
	once bool
	// files maps a path to its content, filled on first use. A tree does
	// not change under us -- the sha names it -- so reading it once is safe
	// and reading it lazily keeps a pass that never asks from paying.
	files map[string]string
	dirs  map[string]bool
	err   error
}

// treeFSPrefixes are the paths treeFS enumerates. Narrow on purpose; see the
// type's doc.
var treeFSPrefixes = []string{"inventory", "deliveries"}

func (t *treeFS) load() error {
	if t.once {
		return t.err
	}
	t.once = true
	t.files = map[string]string{}
	t.dirs = map[string]bool{}

	args := append([]string{"-C", t.g.Dir, "ls-tree", "-r", "--name-only", t.sha, "--"}, treeFSPrefixes...)
	out, err := t.g.run(t.ctx, t.g.Dir, args)
	if err != nil {
		t.err = fmt.Errorf("git ls-tree -r %s: %w", t.sha, err)
		return t.err
	}
	for _, name := range splitLines(out) {
		if name == "" {
			continue
		}
		t.files[name] = ""
		for d := path.Dir(name); d != "." && d != "/"; d = path.Dir(d) {
			t.dirs[d] = true
		}
	}
	return nil
}

func (t *treeFS) Open(name string) (fs.File, error) {
	if err := t.load(); err != nil {
		return nil, err
	}
	if _, ok := t.dirs[name]; ok {
		return &treeDir{fsys: t, name: name}, nil
	}
	if _, ok := t.files[name]; !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	body, err := t.g.run(t.ctx, t.g.Dir, []string{"-C", t.g.Dir, "show", t.sha + ":" + name})
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return &treeFile{name: path.Base(name), r: strings.NewReader(body), size: int64(len(body))}, nil
}

// ReadDir lets fs.ReadDir and fs.WalkDir work, which is what inventory.Load
// uses to enumerate records.
func (t *treeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if err := t.load(); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []fs.DirEntry
	add := func(base string, dir bool) {
		if seen[base] {
			return
		}
		seen[base] = true
		out = append(out, treeEntry{name: base, dir: dir})
	}
	prefix := name + "/"
	if name == "." {
		prefix = ""
	}
	for f := range t.files {
		if !strings.HasPrefix(f, prefix) {
			continue
		}
		rest := strings.TrimPrefix(f, prefix)
		if i := strings.Index(rest, "/"); i >= 0 {
			add(rest[:i], true)
		} else {
			add(rest, false)
		}
	}
	if len(out) == 0 && name != "." && !t.dirs[name] {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// Stat lets fs.Stat answer for a directory, which inventory.Load uses to ask
// whether inventory/ exists at all.
func (t *treeFS) Stat(name string) (fs.FileInfo, error) {
	if err := t.load(); err != nil {
		return nil, err
	}
	if t.dirs[name] {
		return treeEntry{name: path.Base(name), dir: true}, nil
	}
	if _, ok := t.files[name]; ok {
		return treeEntry{name: path.Base(name)}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

type treeEntry struct {
	name string
	dir  bool
}

func (e treeEntry) Name() string { return e.name }
func (e treeEntry) IsDir() bool  { return e.dir }
func (e treeEntry) Type() fs.FileMode {
	if e.dir {
		return fs.ModeDir
	}
	return 0
}
func (e treeEntry) Info() (fs.FileInfo, error) { return e, nil }
func (e treeEntry) Size() int64                { return 0 }
func (e treeEntry) Mode() fs.FileMode          { return e.Type() }
func (e treeEntry) ModTime() time.Time         { return time.Time{} }
func (e treeEntry) Sys() any                   { return nil }

type treeFile struct {
	name string
	r    *strings.Reader
	size int64
}

func (f *treeFile) Stat() (fs.FileInfo, error) { return treeFileInfo{f}, nil }
func (f *treeFile) Read(p []byte) (int, error) { return f.r.Read(p) }
func (f *treeFile) Close() error               { return nil }

type treeFileInfo struct{ f *treeFile }

func (i treeFileInfo) Name() string       { return i.f.name }
func (i treeFileInfo) Size() int64        { return i.f.size }
func (i treeFileInfo) Mode() fs.FileMode  { return 0 }
func (i treeFileInfo) ModTime() time.Time { return time.Time{} }
func (i treeFileInfo) IsDir() bool        { return false }
func (i treeFileInfo) Sys() any           { return nil }

type treeDir struct {
	fsys *treeFS
	name string
}

func (d *treeDir) Stat() (fs.FileInfo, error) {
	return treeEntry{name: path.Base(d.name), dir: true}, nil
}
func (d *treeDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: fs.ErrInvalid}
}
func (d *treeDir) Close() error { return nil }

// TreeFS returns a read-only fs.FS over sha's tree. See treeFS.
func (g execGit) TreeFS(ctx context.Context, sha string) (fs.FS, error) {
	if err := checkRef(sha); err != nil {
		return nil, err
	}
	return &treeFS{g: g, ctx: ctx, sha: sha}, nil
}

// Parent runs `git rev-parse <sha>^` and reports sha's first parent.
//
// ⚠️ A ROOT COMMIT MAKES THAT FAIL, AND THAT IS "NO PARENT", NOT AN ERROR TO
// PROPAGATE. rev-parse on a ref with no parent exits non-zero with "unknown
// revision or path not in the working tree" on stderr; there is nothing
// malformed about the request, there is simply nothing there. Any other
// failure to resolve the parent (git itself misbehaving, an unreadable
// object) is folded into the same false: the caller's contract is "skip the
// comparison", never "refuse the commit for a git problem reading its own
// history" -- see the caller's own doc for why.
func (g execGit) Parent(ctx context.Context, sha string) (string, bool) {
	if err := checkRef(sha); err != nil {
		return "", false
	}
	out, err := g.run(ctx, g.Dir, []string{"-C", g.Dir, "rev-parse", sha + "^"})
	if err != nil {
		return "", false
	}
	lines := splitLines(out)
	if len(lines) != 1 || lines[0] == "" {
		return "", false
	}
	return lines[0], true
}
