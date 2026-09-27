package ground

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Alagroc/uplink/internal/proto"
)

// post sends an authenticated request to one endpoint and returns the outcome.
func post(t *testing.T, base, path, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(payload)
}

// registerOver brings a crew online through the real HTTP path, so the crew id
// it gets is the one a token-scoped request would have to match.
func registerOver(t *testing.T, base, token, name string) string {
	t.Helper()
	body, err := json.Marshal(proto.RegisterReq{
		Name: name, OS: "linux", Arch: "amd64", Hostname: "h", Workdir: "/w", Version: proto.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	status, payload := post(t, base, "/v1/crew/register", token, string(body))
	if status != http.StatusOK {
		t.Fatalf("register %s: status %d: %s", name, status, payload)
	}
	var resp proto.RegisterResp
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.CrewID
}

// The whole point of the change: a crew credential must not reach anything that
// can dispatch work or stop the daemon.
func TestCrewTokenCannotReachOperatorSurfaces(t *testing.T) {
	srv, ts, stopped := newTestServer(t)
	crewToken, err := srv.CrewTokens.Mint("devbox", nil)
	if err != nil {
		t.Fatal(err)
	}

	status, body := post(t, ts.URL, "/mcp", crewToken, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if status != http.StatusUnauthorized {
		t.Errorf("/mcp with a crew token = %d, want 401: %s", status, body)
	}
	if !strings.Contains(body, "crew token") {
		t.Errorf("the refusal should explain the mix-up: %s", body)
	}

	status, body = post(t, ts.URL, "/v1/shutdown", crewToken, `{}`)
	if status != http.StatusUnauthorized {
		t.Errorf("/v1/shutdown with a crew token = %d, want 401: %s", status, body)
	}
	select {
	case <-stopped:
		t.Fatal("a crew token must never be able to stop ground")
	default:
	}
}

// The cutover: the operator token no longer opens crew endpoints, and says so.
func TestOperatorTokenNoLongerOpensCrewEndpoints(t *testing.T) {
	srv, ts, _ := newTestServer(t)
	if _, err := srv.CrewTokens.Mint("devbox", nil); err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(proto.RegisterReq{Name: "devbox", Version: proto.Version})
	if err != nil {
		t.Fatal(err)
	}
	status, payload := post(t, ts.URL, "/v1/crew/register", "operator-token", string(body))
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", status, payload)
	}
	// The message has to name the fix, or this is just a mysterious breakage.
	if !strings.Contains(payload, "crew-token add") {
		t.Errorf("the refusal should name the command that fixes it: %s", payload)
	}
}

// A stolen credential must not be able to masquerade as a different crew.
func TestCrewTokenIsBoundToItsName(t *testing.T) {
	srv, ts, _ := newTestServer(t)
	token, err := srv.CrewTokens.Mint("devbox", nil)
	if err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(proto.RegisterReq{Name: "someone-else", Version: proto.Version})
	if err != nil {
		t.Fatal(err)
	}
	status, payload := post(t, ts.URL, "/v1/crew/register", token, string(body))
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", status, payload)
	}
	if !strings.Contains(payload, "devbox") {
		t.Errorf("the refusal should say which crew the token is for: %s", payload)
	}
}

// The lateral-movement fix. Before crew credentials existed, any crew holding
// the shared token could poll another crew's queue and take its jobs — agent
// tokens included.
func TestCrewCannotTouchAnotherCrewsQueue(t *testing.T) {
	srv, ts, _ := newTestServer(t)
	tokenA, err := srv.CrewTokens.Mint("crew-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	tokenB, err := srv.CrewTokens.Mint("crew-b", nil)
	if err != nil {
		t.Fatal(err)
	}
	idA := registerOver(t, ts.URL, tokenA, "crew-a")
	idB := registerOver(t, ts.URL, tokenB, "crew-b")

	// A job for crew-b, which crew-a must not be able to collect.
	job, err := srv.Ground.Submit(SubmitReq{Crew: "crew-b", Kind: proto.KindExec, Payload: proto.Payload{Command: "true"}})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, path, body string
	}{
		{"poll", "/v1/crew/poll", `{"crew_id":"` + idB + `","wait_ms":10}`},
		{"logs", "/v1/crew/logs", `{"crew_id":"` + idB + `","job_id":"` + job.ID + `","lines":[{"text":"x"}]}`},
		{"state", "/v1/crew/state", `{"crew_id":"` + idB + `","job_id":"` + job.ID + `","state":"done"}`},
	}
	for _, tc := range cases {
		status, payload := post(t, ts.URL, tc.path, tokenA, tc.body)
		if status != http.StatusForbidden {
			t.Errorf("%s against another crew = %d, want 403: %s", tc.name, status, payload)
		}
	}

	// crew-b's own token still works, so the check is not simply refusing all.
	if status, payload := post(t, ts.URL, "/v1/crew/poll", tokenB, `{"crew_id":"`+idB+`","wait_ms":10}`); status != http.StatusOK {
		t.Errorf("the owning crew should still be served, got %d: %s", status, payload)
	}
	// And crew-a can still serve itself.
	if status, payload := post(t, ts.URL, "/v1/crew/poll", tokenA, `{"crew_id":"`+idA+`","wait_ms":10}`); status != http.StatusOK {
		t.Errorf("crew-a polling itself = %d: %s", status, payload)
	}
}

// Minting and revoking must take effect without restarting ground: the store is
// re-read when the file changes.
func TestMintAndRevokeTakeEffectWithoutRestart(t *testing.T) {
	srv, ts, _ := newTestServer(t)

	// A second store over the same file stands in for the CLI process, which is
	// what actually writes in production.
	cli, err := OpenCrewTokens(srv.CrewTokens.Path())
	if err != nil {
		t.Fatal(err)
	}
	token, err := cli.Mint("latecomer", nil)
	if err != nil {
		t.Fatal(err)
	}

	// The running server never restarted, yet must accept it.
	crewID := registerOver(t, ts.URL, token, "latecomer")

	if err := cli.Revoke("latecomer"); err != nil {
		t.Fatal(err)
	}
	status, payload := post(t, ts.URL, "/v1/crew/poll", token, `{"crew_id":"`+crewID+`","wait_ms":10}`)
	if status != http.StatusUnauthorized {
		t.Fatalf("a revoked token = %d, want 401: %s", status, payload)
	}
	if !strings.Contains(payload, "revoked") {
		t.Errorf("the refusal should say it was revoked: %s", payload)
	}
}

// Roles on the token are authoritative, so a crew cannot promote itself into a
// role the operator did not grant.
func TestTokenRolesOverrideWhatTheCrewClaims(t *testing.T) {
	srv, ts, _ := newTestServer(t)
	token, err := srv.CrewTokens.Mint("devbox", []string{"builder"})
	if err != nil {
		t.Fatal(err)
	}

	body, err := json.Marshal(proto.RegisterReq{
		Name: "devbox", Roles: []string{"admin", "reviewer"}, Version: proto.Version,
	})
	if err != nil {
		t.Fatal(err)
	}
	if status, payload := post(t, ts.URL, "/v1/crew/register", token, string(body)); status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, payload)
	}

	crew := srv.Ground.ListCrew()
	if len(crew) != 1 {
		t.Fatalf("expected one crew, got %d", len(crew))
	}
	if len(crew[0].Roles) != 1 || crew[0].Roles[0] != "builder" {
		t.Errorf("roles = %v, want only the granted [builder]", crew[0].Roles)
	}
}

func TestNoCrewTokensYetIsExplained(t *testing.T) {
	_, ts, _ := newTestServer(t)
	body, _ := json.Marshal(proto.RegisterReq{Name: "devbox", Version: proto.Version})
	status, payload := post(t, ts.URL, "/v1/crew/register", CrewTokenPrefix+"nonexistent", string(body))
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if !strings.Contains(payload, "crew-token add") {
		t.Errorf("an empty store should tell the operator how to fill it: %s", payload)
	}
}

// --- store-level behaviour ---

func TestStoreKeepsOnlyHashes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crew-tokens.json")
	s, err := OpenCrewTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.Mint("devbox", []string{"builder"})
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) {
		t.Fatal("the plaintext token was written to disk")
	}
	if !strings.Contains(string(data), "devbox") {
		t.Error("the crew name should be recorded")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %o, want 600", perm)
	}

	// List must not hand back hashes either.
	for _, entry := range s.List() {
		if entry.Hash != "" {
			t.Error("List() exposed a hash")
		}
	}
}

func TestMintRejectsDuplicateUntilRevoked(t *testing.T) {
	s, err := OpenCrewTokens(filepath.Join(t.TempDir(), "crew-tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Mint("devbox", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mint("devbox", nil); err == nil {
		t.Error("minting a second live token for one crew should be refused")
	}

	if err := s.Revoke("devbox"); err != nil {
		t.Fatal(err)
	}
	second, err := s.Mint("devbox", nil)
	if err != nil {
		t.Fatalf("after revoking, re-minting should work: %v", err)
	}
	if second == first {
		t.Error("the replacement must be a different secret")
	}
	// The old one must not come back to life.
	if tok, ok := s.Lookup(first); ok && !tok.Revoked() {
		t.Error("the revoked token is live again")
	}
}

func TestStoreHandlesMissingAndEmptyFile(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenCrewTokens(filepath.Join(dir, "absent.json"))
	if err != nil {
		t.Fatalf("a missing file means nothing minted yet, not an error: %v", err)
	}
	if s.Count() != 0 {
		t.Error("expected an empty store")
	}
	if _, ok := s.Lookup("uplc_whatever"); ok {
		t.Error("an empty store must not authenticate anything")
	}
}

func TestIsCrewToken(t *testing.T) {
	if !IsCrewToken(CrewTokenPrefix + "abc") {
		t.Error("prefixed tokens should be recognised")
	}
	if IsCrewToken("deadbeef") {
		t.Error("a bare operator token is not a crew token")
	}
}
