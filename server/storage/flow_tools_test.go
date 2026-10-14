package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

// toolRig is a flow with one published version and one MCP server offering tools "echo" and "lookup".
type toolRig struct {
	s       *Store
	flowID  string
	server  MCPServer
	hashes  map[string]string
	config  []byte
	version int
}

func newToolRig(t *testing.T, s *Store) *toolRig {
	t.Helper()
	ctx := context.Background()
	m := MCPServer{ID: NewID(), Name: "crm", Endpoint: "https://mcp.example/rpc", Transport: "streamable-http", Enabled: true}
	if err := s.MCP().CreateServer(ctx, m); err != nil {
		t.Fatal(err)
	}
	d, err := s.MCP().AddDiscovery(ctx, m.ID, []MCPTool{
		{RemoteName: "echo", SchemaJSON: []byte(`{"type":"object"}`)},
		{RemoteName: "lookup", SchemaJSON: []byte(`{"type":"object","properties":{"id":{"type":"string"}}}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	hashes := map[string]string{}
	for _, tl := range d.Tools {
		hashes[tl.RemoteName] = tl.SchemaHash
	}
	f := Flow{ID: NewID(), Name: "tools", Enabled: true}
	if err := s.Flows().Create(ctx, f); err != nil {
		t.Fatal(err)
	}
	cfg := []byte(`{"stt":{"provider":"google","model":"s"},"llm":{"provider":"google","model":"l"},"tts":{"provider":"google","voice":"t"}}`)
	if _, err := s.Flows().PublishVersion(ctx, f.ID, 1, cfg, nil); err != nil {
		t.Fatal(err)
	}
	return &toolRig{s: s, flowID: f.ID, server: m, hashes: hashes, config: cfg, version: 1}
}

func (r *toolRig) sel(name string, required bool) SelectedTool {
	return SelectedTool{ServerID: r.server.ID, RemoteName: name, SchemaHash: r.hashes[name], Required: required}
}

// publish runs MutateAndPublish and returns the new current version's selection.
func (r *toolRig) publish(t *testing.T, m FlowMutation) ([]SelectedTool, FlowVersion, error) {
	t.Helper()
	ctx := context.Background()
	v, _, err := r.s.Flows().MutateAndPublish(ctx, r.flowID, m, r.config)
	if err != nil {
		return nil, FlowVersion{}, err
	}
	tools, err := r.s.Flows().SelectedTools(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	return tools, v, nil
}

func TestPublishCanReplaceTheToolSelectionAtomically(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		r := newToolRig(t, s)
		want := []SelectedTool{r.sel("echo", false)}
		got, v, err := r.publish(t, FlowMutation{Tools: &want})
		if err != nil || len(got) != 1 || got[0] != want[0] || v.Version != 2 {
			t.Fatalf("got %+v v%d err %v", got, v.Version, err)
		}
		// nil keeps the previous selection
		got, v, err = r.publish(t, FlowMutation{})
		if err != nil || len(got) != 1 || got[0] != want[0] || v.Version != 3 {
			t.Fatalf("keep: %+v v%d err %v", got, v.Version, err)
		}
		// empty clears it
		none := []SelectedTool{}
		if got, _, err = r.publish(t, FlowMutation{Tools: &none}); err != nil || len(got) != 0 {
			t.Fatalf("clear: %+v err %v", got, err)
		}
	})
}

func TestARejectedSelectionPublishesNothingAndKeepsTheName(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		r := newToolRig(t, s)
		ctx := context.Background()
		stale := r.sel("echo", false)
		stale.SchemaHash = "0000"
		name := "renamed"
		for label, sels := range map[string][]SelectedTool{
			"stale hash":   {stale},
			"unknown tool": {{ServerID: r.server.ID, RemoteName: "nope", SchemaHash: "x"}},
		} {
			sels := sels
			if _, _, err := r.publish(t, FlowMutation{Name: &name, Tools: &sels}); !errors.Is(err, ErrInvalidToolSelection) {
				t.Fatalf("%s: %v", label, err)
			}
		}
		f, err := s.Flows().Current(ctx, r.flowID)
		if err != nil || f.Name != "tools" {
			t.Fatalf("the name changed although nothing was published: %q err %v", f.Name, err)
		}
		v, err := s.Flows().Version(ctx, f.CurrentVersionID)
		if err != nil || v.Version != 1 {
			t.Fatalf("a version was published: %+v err %v", v, err)
		}
	})
}

func TestADisabledServerAndTooManyToolsAreRefused(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		r := newToolRig(t, s)
		many := make([]SelectedTool, 0, MaxSelectedTools+1)
		for i := 0; i <= MaxSelectedTools; i++ {
			many = append(many, r.sel("echo", false))
		}
		if _, _, err := r.publish(t, FlowMutation{Tools: &many}); !errors.Is(err, ErrInvalidToolSelection) {
			t.Fatalf("too many: %v", err)
		}
		if err := s.MCP().DisableServer(context.Background(), r.server.ID); err != nil {
			t.Fatal(err)
		}
		one := []SelectedTool{r.sel("echo", false)}
		if _, _, err := r.publish(t, FlowMutation{Tools: &one}); !errors.Is(err, ErrInvalidToolSelection) {
			t.Fatalf("disabled server: %v", err)
		}
	})
}

func TestChoosingToolsClearsTheReviewFlagAndOtherwiseItCarriesForward(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		r := newToolRig(t, s)
		ctx := context.Background()
		// Detaching a server a flow pins publishes a version that needs review.
		pinned := []SelectedTool{r.sel("echo", false)}
		if _, _, err := r.publish(t, FlowMutation{Tools: &pinned}); err != nil {
			t.Fatal(err)
		}
		if err := s.MCP().DetachAndDelete(ctx, r.server.ID); err != nil {
			t.Fatal(err)
		}
		// The server is gone, so a fresh one stands in for choosing tools again.
		replacement := MCPServer{ID: NewID(), Name: "crm2", Endpoint: "https://mcp.example/rpc2", Transport: "streamable-http", Enabled: true}
		if err := s.MCP().CreateServer(ctx, replacement); err != nil {
			t.Fatal(err)
		}
		d, err := s.MCP().AddDiscovery(ctx, replacement.ID, []MCPTool{{RemoteName: "echo", SchemaJSON: []byte(`{"type":"object"}`)}})
		if err != nil {
			t.Fatal(err)
		}
		r.server, r.hashes = replacement, map[string]string{"echo": d.Tools[0].SchemaHash}
		_, v, err := r.publish(t, FlowMutation{})
		if err != nil || !v.MCPReviewRequired {
			t.Fatalf("a publish without a selection must carry the flag: %+v err %v", v, err)
		}
		one := []SelectedTool{r.sel("echo", true)}
		_, v, err = r.publish(t, FlowMutation{Tools: &one})
		if err != nil || v.MCPReviewRequired {
			t.Fatalf("choosing tools is the review: %+v err %v", v, err)
		}
	})
}

func TestOnlyDraftSessionsMayRunAToolTheVersionLacks(t *testing.T) {
	eachStore(t, func(t *testing.T, s *Store) {
		r := newToolRig(t, s)
		ctx := context.Background()
		cur, err := s.Flows().Current(ctx, r.flowID)
		if err != nil {
			t.Fatal(err)
		}
		v, err := s.Flows().Version(ctx, cur.CurrentVersionID)
		if err != nil {
			t.Fatal(err)
		}
		rec := func(draft bool) SessionRecord {
			return SessionRecord{ID: NewID(), FlowID: r.flowID, FlowVersionID: v.ID, ConfigHash: v.ConfigHash, StartedAt: time.Now(), Status: "active",
				RuntimeTools: []SelectedTool{r.sel("echo", false)}, Draft: draft}
		}
		if err := s.Sessions().Create(ctx, rec(false)); err == nil {
			t.Fatal("an ordinary session ran a tool its version does not select")
		}
		if err := s.Sessions().Create(ctx, rec(true)); err != nil {
			t.Fatalf("a draft session with its own tool: %v", err)
		}
		// A draft still needs the tool to be available now.
		stale := rec(true)
		stale.RuntimeTools[0].SchemaHash = "0000"
		if err := s.Sessions().Create(ctx, stale); err == nil {
			t.Fatal("a draft session started on a schema that changed")
		}
	})
}
