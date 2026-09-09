package parity

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"
)

// The fakes below stand at the PROCESS boundary, not at a Go interface:
// truss runs as a real subprocess here, so everything it depends on has to
// be something a separate process can reach. Each one answers out of the
// same scenario the reference PATH shims answer out of, and each one is
// deliberately as strict as the thing it replaces -- a fake more forgiving
// than production invents failures and a stricter one hides them, which is
// this repo's own standing rule and the reason the list endpoint below
// strips `merged`.

// --- the ledger, an S3-compatible bucket ------------------------------------

// fakeLedger serves GET and PUT path-style, which is the addressing the
// real endpoint was measured to want (§8). It is the harness's copy of the
// bucket: what the reference shim keeps as a directory tree, this keeps as
// a map, and both are compared key by key at the end.
type fakeLedger struct {
	mu      sync.Mutex
	bucket  string
	objects map[string][]byte
	srv     *httptest.Server
}

func newFakeLedger(bucket string, seed map[string][]byte) *fakeLedger {
	f := &fakeLedger{bucket: bucket, objects: map[string][]byte{}}
	for k, v := range seed {
		f.objects[k] = append([]byte(nil), v...)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeLedger) close() { f.srv.Close() }

// snapshot is the bucket at the end of the pass, in the corpus's own
// encoding, ready to compare against what the bash left behind.
func (f *fakeLedger) snapshot() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.objects))
	for k, v := range f.objects {
		out[k] = Encode(v)
	}
	return out
}

func (f *fakeLedger) handle(w http.ResponseWriter, r *http.Request) {
	prefix := "/" + f.bucket + "/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, prefix)

	switch r.Method {
	case http.MethodGet:
		f.mu.Lock()
		body, ok := f.objects[key]
		f.mu.Unlock()
		if !ok {
			// NoSuchKey, in the XML shape the real endpoint sends, so
			// ledger.Store's "absent" is reached the way it is in
			// production rather than by a bare status code (§3.2 turns
			// exactly this into an exit code of its own).
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			_ = xml.NewEncoder(w).Encode(struct {
				XMLName xml.Name `xml:"Error"`
				Code    string   `xml:"Code"`
				Message string   `xml:"Message"`
			}{Code: "NoSuchKey", Message: "The specified key does not exist."})
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.objects[key] = body
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// --- the forge, GitHub's API ------------------------------------------------

// newFakeForge answers the seven endpoints internal/forge calls, out of the
// scenario's own fixtures. Anything else is a 404, so an endpoint added to
// the client without being added here fails loudly instead of decoding a
// zero value.
func newFakeForge(f Fixtures, mintToken string) *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/app/installations/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      mintToken,
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})

	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasSuffix(p, "/branches/main/protection"):
			body := f.BranchProtection
			if len(body) == 0 {
				body = json.RawMessage(`{}`)
			}
			_, _ = w.Write(body)

		case strings.Contains(p, "/rules/branches/"):
			// ⚠️ ALWAYS "NO RULESETS APPLY", BECAUSE apply.sh NEVER READ ONE.
			// The recorded corpus (internal/parity's whole reason to exist)
			// has no ruleset fixtures to replay -- the bash this project
			// ports never called this endpoint, so there is nothing to
			// record. An empty list is a real, compliant answer
			// (gates.CheckRulesets has nothing to refuse on it), not a stand-in
			// for "unreadable"; if a scenario ever needs a ruleset with a
			// bypass actor, it needs a fixture field added here, deliberately,
			// not a fake answering something the recording never saw.
			_, _ = w.Write([]byte(`[]`))

		case strings.Contains(p, "/commits/") && strings.HasSuffix(p, "/pulls"):
			sha := between(p, "/commits/", "/pulls")
			// ⚠️ `merged` IS STRIPPED, exactly as the reference shim
			// strips it. GitHub's list endpoint carries only `merged_at`,
			// and a fixture that answered `merged` here would model a
			// response GitHub never sends -- the mistake that let a gate
			// rejecting every merged PR pass its own tests
			// (beeradb/platform#1, 2026-09-07).
			items := []map[string]any{}
			for _, pr := range f.PRs[sha] {
				item := map[string]any{
					"number":           pr.Number,
					"merge_commit_sha": pr.MergeCommitSHA,
					"head":             map[string]any{"sha": pr.Head.SHA},
					"merged_at":        nil,
				}
				if pr.Merged {
					item["merged_at"] = "2026-09-07T00:00:00Z"
				}
				items = append(items, item)
			}
			_ = json.NewEncoder(w).Encode(items)

		case strings.Contains(p, "/pulls/") && strings.HasSuffix(p, "/reviews"):
			number := between(p, "/pulls/", "/reviews")
			out := []map[string]any{}
			for _, rv := range f.Reviews[number] {
				out = append(out, map[string]any{
					"user":         map[string]any{"login": rv.User.Login},
					"state":        rv.State,
					"commit_id":    rv.CommitID,
					"submitted_at": time.Now().UTC().Format(time.RFC3339),
				})
			}
			_ = json.NewEncoder(w).Encode(out)

		case strings.Contains(p, "/pulls/"):
			number := p[strings.LastIndex(p, "/pulls/")+len("/pulls/"):]
			for _, byCommit := range f.PRs {
				for _, pr := range byCommit {
					if fmt.Sprint(pr.Number) != number {
						continue
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"number":           pr.Number,
						"merged":           pr.Merged,
						"merge_commit_sha": pr.MergeCommitSHA,
						"head":             map[string]any{"sha": pr.Head.SHA},
					})
					return
				}
			}
			// The reference shim answers `{}` for a PR nobody declared,
			// which the gate then refuses for having no merge commit.
			_, _ = w.Write([]byte(`{}`))

		case strings.Contains(p, "/commits/"):
			sha := p[strings.LastIndex(p, "/commits/")+len("/commits/"):]
			if body, ok := f.CommitVerify[sha]; ok {
				// ⚠️ `sha` IS INJECTED WHEN THE FIXTURE OMITS IT, BECAUSE
				// THE REAL ENDPOINT ALWAYS SENDS IT. The reference
				// fixtures leave it out -- apply.sh never reads it, it
				// prints its own loop variable -- and truss's refusal
				// names gates.Commit.SHA, which came from the wire. A
				// fixture missing a field production always carries made
				// truss look like it had lost the sha; the fixture was the
				// bug, which is this repo's own standing rule about a
				// fixture that disagrees with production.
				_, _ = w.Write(withSHA(body, sha))
				return
			}
			_, _ = w.Write(withSHA([]byte(`{"commit":{"verification":{"verified":true}},"committer":{"login":"web-flow"}}`), sha))

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	return httptest.NewServer(mux)
}

// withSHA adds the commit endpoint's own `sha` field to a fixture body
// that does not carry one. It never overwrites one that is there.
func withSHA(body []byte, sha string) []byte {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return body
	}
	if _, ok := parsed["sha"]; ok {
		return body
	}
	parsed["sha"] = sha
	out, err := json.Marshal(parsed)
	if err != nil {
		return body
	}
	return out
}

func between(s, open, close string) string {
	i := strings.Index(s, open)
	if i < 0 {
		return ""
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return rest
	}
	return rest[:j]
}

// --- Vault, the expiry metadata store ---------------------------------------

// newFakeVault answers a Kubernetes-auth login, a metadata LIST, and one
// metadata read per item. items maps a title to its custom_metadata; a nil
// map is an item that records no `expires` at all, which is what
// production looks like today and is the state the sweep's own alarm
// exists for.
func newFakeVault(mount string, items map[string]map[string]string) *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/auth/kubernetes/login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"auth": map[string]any{"client_token": "fake-vault-tok"},
		})
	})

	mux.HandleFunc("/v1/"+mount+"/metadata", func(w http.ResponseWriter, r *http.Request) {
		if len(items) == 0 {
			// Vault's own way of saying "zero items".
			w.WriteHeader(http.StatusNotFound)
			return
		}
		keys := make([]string, 0, len(items))
		for k := range items {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"keys": keys}})
	})

	mux.HandleFunc("/v1/"+mount+"/metadata/", func(w http.ResponseWriter, r *http.Request) {
		item := strings.TrimPrefix(r.URL.Path, "/v1/"+mount+"/metadata/")
		cm, ok := items[item]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		custom := map[string]any{}
		for k, v := range cm {
			custom[k] = v
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"custom_metadata": custom},
		})
	})

	return httptest.NewServer(mux)
}

// --- Cloudflare, the one issuer that is asked directly ----------------------

// newFakeCloudflare answers the token-verify endpoint the expiry sweep
// probes for the hand-made minting token. With no stub it answers
// `{"ok":true}` -- a 200 naming no expiry -- which is what the reference
// `curl` shim falls through to and what the probe must read as "no expiry
// recorded" rather than as an error.
func newFakeCloudflare(v *CloudflareVerify, now time.Time) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case v == nil:
			_, _ = w.Write([]byte(`{"ok":true}`))
		case v.Unreadable:
			_, _ = w.Write([]byte(`not json`))
		case v.ExpiresOn == "":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  map[string]any{"status": "active"},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result": map[string]any{
					"status":     "active",
					"expires_on": ResolveDate(now, v.ExpiresOn),
				},
			})
		}
	}))
}

// newRefusingProxy is where the Telegram send goes.
//
// notify.Telegram's endpoint is a literal host with no override, so a
// subprocess cannot be pointed at a fake one -- and the alert this harness
// compares is read off truss's stdout, which carries the same string Send
// would post. What is left to arrange is that the send neither reaches the
// internet nor waits on it: Go's HTTP transport honours $HTTPS_PROXY, and
// a proxy that refuses every CONNECT fails the request immediately and
// locally. No test in this package egresses, and none waits on a DNS
// timeout for a delivery whose success is not what §5.5 asks about.
//
// It is a server rather than a closed port on a written-down address
// because scripts/leakscan refuses an IP address anywhere in a tracked
// file, correctly and without an exception for loopback -- and a computed
// httptest URL is not a written-down address.
func newRefusingProxy() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "parity: outbound requests are refused", http.StatusForbidden)
	}))
}
