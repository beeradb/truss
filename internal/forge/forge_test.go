package forge

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- fixtures ---------------------------------------------------------------

// testKey generates a fresh RSA key at test time and returns it alongside
// its PEM encoding, so no fixture in this package ever embeds a real (or
// even a fake-but-permanent) private key -- leakscan's rule and simple good
// practice agree here.
func testKey(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: der}
	return key, pem.EncodeToMemory(block)
}

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	_, keyPEM := testKey(t)
	c, err := New(Config{
		BaseURL:        baseURL,
		Repo:           "acme/widgets",
		AppID:          123456,
		InstallationID: 7654321,
		PrivateKeyPEM:  keyPEM,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// mintHandler answers /app/installations/.../access_tokens with a fixed
// token, so tests that only care about the authenticated calls downstream
// don't each need to reimplement the mint.
func mintHandler(token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(installTokenResponse{
			Token:     token,
			ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}
}

// --- New ----------------------------------------------------------------

func TestNewRefusesAnEmptyAppIDOrKey(t *testing.T) {
	_, keyPEM := testKey(t)
	base := Config{
		Repo:           "acme/widgets",
		AppID:          1,
		InstallationID: 1,
		PrivateKeyPEM:  keyPEM,
	}

	if _, err := New(base); err != nil {
		t.Fatalf("the valid baseline config was refused: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"zero app id", func(c *Config) { c.AppID = 0 }},
		{"negative app id", func(c *Config) { c.AppID = -1 }},
		{"zero installation id", func(c *Config) { c.InstallationID = 0 }},
		{"empty repo", func(c *Config) { c.Repo = "" }},
		{"repo with no slash", func(c *Config) { c.Repo = "acme" }},
		{"empty key", func(c *Config) { c.PrivateKeyPEM = nil }},
		{"garbage key", func(c *Config) { c.PrivateKeyPEM = []byte("not a pem") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatalf("New accepted an invalid config: %+v", cfg)
			}
		})
	}
}

func TestARepoNameCannotTraverseTheURLPath(t *testing.T) {
	_, keyPEM := testKey(t)
	base := Config{AppID: 1, InstallationID: 1, PrivateKeyPEM: keyPEM}

	bad := []string{
		"../../etc/passwd",
		"acme/..",
		"../acme",
		"acme/widgets/../../secret",
		"acme/wid gets",
		"acme/widgets/",
		"/acme/widgets",
	}
	for _, repo := range bad {
		t.Run(repo, func(t *testing.T) {
			cfg := base
			cfg.Repo = repo
			if _, err := New(cfg); err == nil {
				t.Fatalf("New accepted repo %q, which could traverse a URL path", repo)
			}
		})
	}

	cfg := base
	cfg.Repo = "acme/widgets"
	if _, err := New(cfg); err != nil {
		t.Fatalf("New refused an ordinary owner/name repo: %v", err)
	}
}

// --- InstallationToken / the JWT --------------------------------------------

func TestInstallationTokenSignsAnRS256JWT(t *testing.T) {
	key, keyPEM := testKey(t)

	var gotAuth string
	before := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/7654321/access_tokens" {
			t.Errorf("unexpected mint request: %s %s", r.Method, r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(installTokenResponse{
			Token:     "ghs_faketoken",
			ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	defer srv.Close()

	c, err := New(Config{
		BaseURL:        srv.URL,
		Repo:           "acme/widgets",
		AppID:          123456,
		InstallationID: 7654321,
		PrivateKeyPEM:  keyPEM,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	token, expiry, err := c.InstallationToken(context.Background())
	after := time.Now()
	if err != nil {
		t.Fatalf("InstallationToken: %v", err)
	}
	if token != "ghs_faketoken" {
		t.Fatalf("InstallationToken returned %q, want the minted token", token)
	}
	if expiry.Before(after) {
		t.Fatalf("InstallationToken returned an expiry in the past")
	}

	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Fatalf("mint request Authorization header was %q, want a Bearer JWT", gotAuth)
	}
	jwt := strings.TrimPrefix(gotAuth, "Bearer ")
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d segments, want 3: %q", len(parts), jwt)
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("decoding jwt header: %v", err)
	}
	var header struct{ Alg, Typ string }
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		t.Fatalf("unmarshalling jwt header: %v", err)
	}
	if header.Alg != "RS256" || header.Typ != "JWT" {
		t.Fatalf("jwt header = %+v, want RS256/JWT", header)
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding jwt payload: %v", err)
	}
	var payload struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("unmarshalling jwt payload: %v", err)
	}
	if payload.Iss != "123456" {
		t.Fatalf("jwt iss = %q, want \"123456\" (a JSON string, matching gh-app-token's own printf)", payload.Iss)
	}
	// iat is backdated 60s from some `now` between before/after; exp is that
	// same now + 540s. Bound both against the window this test ran in.
	wantIatMin, wantIatMax := before.Unix()-60, after.Unix()-60
	if payload.Iat < wantIatMin || payload.Iat > wantIatMax {
		t.Fatalf("jwt iat = %d, want between %d and %d", payload.Iat, wantIatMin, wantIatMax)
	}
	if payload.Exp != payload.Iat+600 {
		t.Fatalf("jwt exp = %d, iat = %d: want exp exactly 600s after iat (60s backdate + 540s forward)", payload.Exp, payload.Iat)
	}

	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decoding jwt signature: %v", err)
	}
	digest := sha256Sum(parts[0] + "." + parts[1])
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], sigBytes); err != nil {
		t.Fatalf("jwt signature does not verify against the key that was configured: %v", err)
	}
}

func sha256Sum(s string) [32]byte {
	h := crypto.SHA256.New()
	h.Write([]byte(s))
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func TestInstallationTokenFailsOnAnEmptyTokenNamingTheMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "installation not found"})
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, _, err := c.InstallationToken(context.Background())
	if err == nil {
		t.Fatal("an empty token was accepted")
	}
	if !strings.Contains(err.Error(), "installation not found") {
		t.Fatalf("error %q does not name the API's message", err)
	}
}

// --- Protection --------------------------------------------------------------

func TestProtectionKeepsAbsentAsAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		case "/repos/acme/widgets/branches/main/protection":
			// No allow_force_pushes key at all, no required_status_checks
			// key at all -- everything below them must decode to nil, not
			// to a compliant-looking false/true.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"required_pull_request_reviews": {
					"required_approving_review_count": 1,
					"require_code_owner_reviews": true,
					"dismiss_stale_reviews": true
				},
				"enforce_admins": {"enabled": true}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	p, err := c.Protection(context.Background(), "main")
	if err != nil {
		t.Fatalf("Protection: %v", err)
	}
	if p.AllowForcePushes != nil {
		t.Fatalf("AllowForcePushes = %v, want nil for an absent key", *p.AllowForcePushes)
	}
	if p.RequireUpToDateBranch != nil {
		t.Fatalf("RequireUpToDateBranch = %v, want nil for an absent required_status_checks", *p.RequireUpToDateBranch)
	}
	if p.StatusChecks != nil {
		t.Fatalf("StatusChecks = %v, want nil/empty for an absent required_status_checks", p.StatusChecks)
	}
	if p.RequiredApprovals == nil || *p.RequiredApprovals != 1 {
		t.Fatalf("RequiredApprovals = %v, want 1 (it WAS present)", p.RequiredApprovals)
	}
	if p.AllowDeletions != nil {
		t.Fatalf("AllowDeletions = %v, want nil for an absent key", *p.AllowDeletions)
	}
	if p.RequireLastPushApproval != nil {
		t.Fatalf("RequireLastPushApproval = %v, want nil for an absent key", *p.RequireLastPushApproval)
	}
	// ⚠️ THE ONE FIELD WHERE ABSENT MUST DECODE TO NIL FOR THE OPPOSITE
	// REASON FROM EVERY OTHER FIELD IN THIS TEST: nil here is what makes the
	// repository COMPLIANT, not what makes it unreadable.
	if p.BypassPullRequestAllowances != nil {
		t.Fatalf("BypassPullRequestAllowances = %+v, want nil for an absent key", p.BypassPullRequestAllowances)
	}
}

// TestProtectionReadsTheThreeAddedFieldsWhenPresent decodes
// allow_deletions, require_last_push_approval, and
// bypass_pull_request_allowances the way GitHub sends them when they ARE
// set, mirroring TestProtectionKeepsAbsentAsAbsent's coverage of when they
// are not.
func TestProtectionReadsTheThreeAddedFieldsWhenPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		case "/repos/acme/widgets/branches/main/protection":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"required_pull_request_reviews": {
					"required_approving_review_count": 1,
					"require_code_owner_reviews": true,
					"dismiss_stale_reviews": true,
					"require_last_push_approval": true,
					"bypass_pull_request_allowances": {
						"users": ["alice"],
						"teams": [],
						"apps": ["some-app"]
					}
				},
				"enforce_admins": {"enabled": true},
				"allow_deletions": {"enabled": true}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	p, err := c.Protection(context.Background(), "main")
	if err != nil {
		t.Fatalf("Protection: %v", err)
	}
	if p.AllowDeletions == nil || *p.AllowDeletions != true {
		t.Fatalf("AllowDeletions = %v, want true (it WAS present)", p.AllowDeletions)
	}
	if p.RequireLastPushApproval == nil || *p.RequireLastPushApproval != true {
		t.Fatalf("RequireLastPushApproval = %v, want true (it WAS present)", p.RequireLastPushApproval)
	}
	if p.BypassPullRequestAllowances == nil {
		t.Fatal("BypassPullRequestAllowances = nil, want the users/apps it was set with")
	}
	if len(p.BypassPullRequestAllowances.Users) != 1 || p.BypassPullRequestAllowances.Users[0] != "alice" {
		t.Fatalf("BypassPullRequestAllowances.Users = %v, want [alice]", p.BypassPullRequestAllowances.Users)
	}
	if len(p.BypassPullRequestAllowances.Apps) != 1 || p.BypassPullRequestAllowances.Apps[0] != "some-app" {
		t.Fatalf("BypassPullRequestAllowances.Apps = %v, want [some-app]", p.BypassPullRequestAllowances.Apps)
	}
}

func TestProtectionReadsBothStatusCheckShapes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		case "/repos/acme/widgets/branches/main/protection":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"required_status_checks": {
					"strict": true,
					"contexts": ["lint"],
					"checks": [{"context": "plan", "app_id": 99}]
				}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	p, err := c.Protection(context.Background(), "main")
	if err != nil {
		t.Fatalf("Protection: %v", err)
	}
	want := map[string]bool{"lint": true, "plan": true}
	got := map[string]bool{}
	for _, s := range p.StatusChecks {
		got[s] = true
	}
	if len(got) != len(want) {
		t.Fatalf("StatusChecks = %v, want both the contexts entry and the checks[].context entry", p.StatusChecks)
	}
	for name := range want {
		if !got[name] {
			t.Fatalf("StatusChecks = %v, missing %q", p.StatusChecks, name)
		}
	}
}

func TestANon2xxIsAnErrorNotAZeroValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message": "internal error"}`))
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	p, err := c.Protection(context.Background(), "main")
	if err == nil {
		t.Fatalf("a 500 was not reported as an error; got zero-value Protection = %+v", p)
	}
}

func TestProtectionIsReadOncePerPass(t *testing.T) {
	var protectionCalls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		case "/repos/acme/widgets/branches/main/protection":
			atomic.AddInt64(&protectionCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"required_pull_request_reviews":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := c.Protection(context.Background(), "main"); err != nil {
		t.Fatalf("Protection: %v", err)
	}
	if got := atomic.LoadInt64(&protectionCalls); got != 1 {
		t.Fatalf("branch protection endpoint was hit %d times for one Protection call, want 1", got)
	}
}

// --- PullNumbersForCommit / PullRequest (the merged-endpoint guard) --------

func TestMergedComesFromTheDetailEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		case r.URL.Path == "/repos/acme/widgets/commits/headsha1/pulls":
			// Real GitHub shape: no "merged" field here, only "merged_at".
			// This handler additionally plants a WRONG "merged": false, so a
			// decoder that (incorrectly) tried to read it would visibly
			// disagree with the detail endpoint below instead of coincidentally matching it.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"number": 42, "merged_at": "2026-09-01T00:00:00Z", "merged": false}]`))
		case r.URL.Path == "/repos/acme/widgets/pulls/42":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"number": 42,
				"merged": true,
				"merge_commit_sha": "headsha1",
				"head": {"sha": "prheadsha1"}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)

	numbers, err := c.PullNumbersForCommit(context.Background(), "headsha1")
	if err != nil {
		t.Fatalf("PullNumbersForCommit: %v", err)
	}
	if len(numbers) != 1 || numbers[0] != 42 {
		t.Fatalf("PullNumbersForCommit = %v, want [42]", numbers)
	}

	pr, err := c.PullRequest(context.Background(), numbers[0])
	if err != nil {
		t.Fatalf("PullRequest: %v", err)
	}
	if !pr.Merged {
		t.Fatalf("PullRequest.Merged = false, want true (from the detail endpoint, ignoring the list endpoint's wrong value)")
	}
	if pr.MergeCommitSHA != "headsha1" {
		t.Fatalf("PullRequest.MergeCommitSHA = %q, want headsha1", pr.MergeCommitSHA)
	}
	if pr.HeadSHA != "prheadsha1" {
		t.Fatalf("PullRequest.HeadSHA = %q, want prheadsha1", pr.HeadSHA)
	}
}

func TestReviewsMapsUserStateAndCommit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		case "/repos/acme/widgets/pulls/9/reviews":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"user": {"login": "alice"}, "state": "APPROVED", "commit_id": "headsha1", "submitted_at": "2026-09-01T00:00:00Z"}
			]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	reviews, err := c.Reviews(context.Background(), 9)
	if err != nil {
		t.Fatalf("Reviews: %v", err)
	}
	if len(reviews) != 1 {
		t.Fatalf("Reviews = %v, want 1 entry", reviews)
	}
	r := reviews[0]
	if r.User != "alice" || r.State != "APPROVED" || r.CommitID != "headsha1" {
		t.Fatalf("Reviews[0] = %+v, want alice/APPROVED/headsha1", r)
	}
}

// --- Commit -------------------------------------------------------------

func TestCommitKeepsAbsentVerificationAndCommitterAsAbsent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		case "/repos/acme/widgets/commits/sha1":
			w.Header().Set("Content-Type", "application/json")
			// committer is JSON null: GitHub could not match the commit to
			// an account. commit.verification is present but "verified" is
			// absent.
			_, _ = w.Write([]byte(`{
				"sha": "sha1",
				"commit": {"verification": {}},
				"committer": null
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	commit, err := c.Commit(context.Background(), "sha1")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if commit.Verified != nil {
		t.Fatalf("Verified = %v, want nil for an absent key", *commit.Verified)
	}
	if commit.CommitterLogin != nil {
		t.Fatalf("CommitterLogin = %v, want nil for a null committer", *commit.CommitterLogin)
	}
}

func TestCommitReadsAVerifiedWebFlowMerge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		case "/repos/acme/widgets/commits/sha1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"sha": "sha1",
				"commit": {"verification": {"verified": true}},
				"committer": {"login": "web-flow"}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	commit, err := c.Commit(context.Background(), "sha1")
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if commit.Verified == nil || !*commit.Verified {
		t.Fatalf("Verified = %v, want true", commit.Verified)
	}
	if commit.CommitterLogin == nil || *commit.CommitterLogin != "web-flow" {
		t.Fatalf("CommitterLogin = %v, want web-flow", commit.CommitterLogin)
	}
}

// --- credential hygiene -------------------------------------------------

// TestNoTokenAppearsInAnyError is the other load-bearing guard: a 401 whose
// body echoes the installation token (or the JWT used to mint it) must not
// produce an error containing that value, whether the leak would come
// through the response body or, per net/http embedding the request URL in
// *url.Error, through a bare wrap of the transport error.
func TestNoTokenAppearsInAnyError(t *testing.T) {
	const plantedCredential = "ghs_" + "planted" + "000111222" // assembled so it is not one literal

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/7654321/access_tokens":
			mintHandler(plantedCredential)(w, r)
		case "/repos/acme/widgets/branches/main/protection":
			auth := r.Header.Get("Authorization")
			w.WriteHeader(http.StatusUnauthorized)
			// A body that echoes back exactly what the (bad) credential in
			// the Authorization header was -- the shape the task warns
			// about: "a 401 whose body echoes the credential".
			_, _ = fmt.Fprintf(w, `{"message": "Bad credentials, saw header: %s"}`, auth)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, err := c.Protection(context.Background(), "main")
	if err == nil {
		t.Fatal("a 401 was not reported as an error")
	}
	if strings.Contains(err.Error(), plantedCredential) {
		t.Fatalf("error contains the installation token: %v", err)
	}
}

// TestNoTokenAppearsInAnyErrorFromTheMintItself covers the JWT specifically,
// since it is a different secret (used before any installation token
// exists) and is built entirely inside this package.
func TestNoTokenAppearsInAnyErrorFromTheMintItself(t *testing.T) {
	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprintf(w, `{"message": "Bad credentials, saw: %s"}`, capturedAuth)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_, _, err := c.InstallationToken(context.Background())
	if err == nil {
		t.Fatal("a 401 from the mint endpoint was not reported as an error")
	}
	jwt := strings.TrimPrefix(capturedAuth, "Bearer ")
	if jwt == "" {
		t.Fatal("test setup did not capture a JWT")
	}
	if strings.Contains(err.Error(), jwt) {
		t.Fatalf("error contains the App JWT: %v", err)
	}
}

// --- New()'s repo/branch escaping shows up on the wire ----------------------

func TestBranchNameIsEscapedInTheRequestPath(t *testing.T) {
	// r.URL.Path is always the DECODED form, regardless of what actually
	// went out on the wire -- so the assertion below reads r.RequestURI,
	// which is the raw, unparsed request-line path, to see whether "/" was
	// really sent as "%2F".
	var gotRequestURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/app/installations/7654321/access_tokens":
			mintHandler("ghs_x")(w, r)
		case strings.HasPrefix(r.URL.Path, "/repos/acme/widgets/branches/"):
			gotRequestURI = r.RequestURI
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if _, err := c.Protection(context.Background(), "release/2026-09"); err != nil {
		t.Fatalf("Protection: %v", err)
	}
	// url.PathEscape turns "/" into "%2F", matching what GitHub's own docs
	// require for a branch name containing one.
	want := "/repos/acme/widgets/branches/release%2F2026-09/protection"
	if gotRequestURI != want {
		t.Fatalf("request-line path = %q, want %q", gotRequestURI, want)
	}
}
