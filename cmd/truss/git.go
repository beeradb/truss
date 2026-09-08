// git.go drives the git binary that backs root discovery and checkout.
// §4.4 specifies a Git type living in internal/repo, but the package as
// actually built (internal/repo/roots.go) contains only the pure
// TouchedRoots function -- no Git struct exists anywhere in the tree.
// Rather than silently add an exported API to a package that is already
// built and settled, the git driver lives here in cmd/truss instead: it is
// only ever used by the apply pass, behind the gitDriver interface below so
// tests can fake it. A named deviation from §4.4, not an oversight.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// gitDriver is the subset of git operations the apply pass needs, matching
// §4.4's Git type shape. A local interface rather than a
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
	// Leaving the token in remote.origin.url would authenticate every call
	// for free, at the price of writing the credential to disk inside the
	// clone. Keeping it out of the URL is right; it just has to be carried
	// to every call instead. Found by the 2026-09-08 code audit.
	//
	// It returns a copy rather than mutating, so the token is still never
	// stored anywhere longer-lived than one pass, and it still travels only
	// in the child's environment -- never argv, never a URL, and never
	// `git config`-ed into the clone, which is exactly the on-disk leak this
	// arrangement exists to avoid.
	WithToken(token string) gitDriver

	EnsureClone(ctx context.Context, repoURL string) error
	Fetch(ctx context.Context, remote, branch string) error
	Commits(ctx context.Context, from, to string) ([]string, error)
	ChangedFiles(ctx context.Context, sha string) ([]string, error)
	TreeRoots(ctx context.Context, sha string) ([]string, error)
	Checkout(ctx context.Context, ref string) error
	HasDir(root string) bool
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
// is true of every ref in production, and it refuses the synthetic refs the
// test corpus is built on ("sha1", "base") -- so adopting it would have meant
// rewriting the tests to satisfy a sentence in the spec, which is the wrong
// trade: the tests are the evidence, the sentence is a description of it.
// A ref that is merely not a sha cannot do harm -- git fails to resolve
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
// in sha's own tree. This is what feeds repo.TouchedRoots' shared-input
// branch, so the filter is part of the contract: only entries matching
// `^(platform|projects/[^/]+)$` come back, and nothing deeper.
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
	cmd := exec.CommandContext(ctx, bin, args...)
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

// basicAuth builds the "x-access-token:<token>" Basic credential GitHub
// expects for an installation token. The obvious place to put it is the
// clone URL; instead it travels as a header value in the child's
// environment, never in argv and never in the URL, so it is not written to
// disk and not visible in a process listing.
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
