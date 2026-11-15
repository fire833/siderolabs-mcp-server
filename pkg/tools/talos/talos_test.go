// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

package talos_test

import (
	"crypto/tls"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	talosclient "github.com/siderolabs/talos/pkg/machinery/client"

	"github.com/siderolabs/mcp-server/pkg/tools/talos"
)

func connect(t *testing.T, routes talos.Backends) *mcp.ClientSession {
	t.Helper()

	server := mcp.NewServer(&mcp.Implementation{Name: "test"}, nil)
	talos.Register(server, routes)

	st, ct := mcp.NewInMemoryTransports()

	if _, err := server.Connect(t.Context(), st, nil); err != nil {
		t.Fatal(err)
	}

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { cs.Close() }) //nolint:errcheck

	return cs
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}

	return res.Content[0].(*mcp.TextContent).Text, res.IsError //nolint:errcheck,forcetypeassert
}

func TestDisabledBackends(t *testing.T) {
	cs := connect(t, talos.Backends{})

	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{args: map[string]any{"nodes": []string{"10.5.0.2"}}, want: "direct Talos access is disabled"},
		{args: map[string]any{"nodes": []string{"10.5.0.2"}, "cluster": "dev"}, want: "Omni access is disabled"},
		{args: map[string]any{"nodes": []string{}}, want: "nodes"},
	} {
		out, isErr := call(t, cs, "talos_version", tc.args)
		if !isErr || !strings.Contains(out, tc.want) {
			t.Errorf("talos_version(%v) = %q (error %v), want error containing %q", tc.args, out, isErr, tc.want)
		}
	}
}

// TestUnreachableNode checks that per-node failures are reported inline instead of failing the call.
func TestUnreachableNode(t *testing.T) {
	c, err := talosclient.New(t.Context(),
		talosclient.WithEndpoints("127.0.0.1:1"),
		talosclient.WithTLSConfig(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec
	)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { c.Close() }) //nolint:errcheck

	cs := connect(t, talos.Backends{Direct: c})
	nodes := []string{"10.5.0.2", "10.5.0.3"}

	for _, tc := range []struct {
		args map[string]any
		tool string
	}{
		{tool: "talos_version", args: map[string]any{"nodes": nodes}},
		{tool: "talos_get", args: map[string]any{"nodes": nodes, "type": "members"}},
		{tool: "talos_logs", args: map[string]any{"nodes": nodes, "service": "kubelet"}},
		{tool: "talos_dmesg", args: map[string]any{"nodes": nodes}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			out, isErr := call(t, cs, tc.tool, tc.args)
			if isErr {
				t.Fatalf("%s failed: %s", tc.tool, out)
			}

			for _, node := range nodes {
				if !strings.Contains(out, "--- node "+node+" ---\nerror: ") {
					t.Errorf("%s output lacks error for node %s: %q", tc.tool, node, out)
				}
			}
		})
	}
}

func TestTailLinesBounds(t *testing.T) {
	cs := connect(t, talos.Backends{})

	for _, n := range []int{0, 10001} {
		if out, isErr := call(t, cs, "talos_dmesg", map[string]any{"nodes": []string{"a"}, "tail_lines": n}); !isErr {
			t.Errorf("tail_lines %d accepted: %q", n, out)
		}
	}
}
