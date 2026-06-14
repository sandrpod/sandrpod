package mcpbridge

import "testing"

func TestServerConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		cfg     ServerConfig
		wantErr bool
	}{
		{"command only", ServerConfig{Command: "npx"}, false},
		{"url only", ServerConfig{URL: "https://x/mcp"}, false},
		{"both", ServerConfig{Command: "npx", URL: "https://x/mcp"}, true},
		{"neither", ServerConfig{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if (err != nil) != c.wantErr {
				t.Fatalf("Validate()=%v, wantErr=%v", err, c.wantErr)
			}
		})
	}
}

func TestServerConfig_IsHTTPAndTarget(t *testing.T) {
	stdio := ServerConfig{Command: "uvx", Args: []string{"x"}}
	http := ServerConfig{URL: "https://h/mcp"}
	if stdio.IsHTTP() || stdio.Target() != "uvx" {
		t.Fatalf("stdio: IsHTTP=%v target=%q", stdio.IsHTTP(), stdio.Target())
	}
	if !http.IsHTTP() || http.Target() != "https://h/mcp" {
		t.Fatalf("http: IsHTTP=%v target=%q", http.IsHTTP(), http.Target())
	}
}

func TestHashServerConfig_DistinguishesHTTPFields(t *testing.T) {
	base := ServerConfig{URL: "https://h/mcp", Type: "streamable-http", Headers: map[string]string{"A": "1"}}
	same := ServerConfig{URL: "https://h/mcp", Type: "streamable-http", Headers: map[string]string{"A": "1"}}
	if hashServerConfig(base) != hashServerConfig(same) {
		t.Fatal("identical HTTP configs should hash equal")
	}
	for _, mut := range []ServerConfig{
		{URL: "https://h/mcp2", Type: "streamable-http", Headers: map[string]string{"A": "1"}}, // url
		{URL: "https://h/mcp", Type: "sse", Headers: map[string]string{"A": "1"}},              // type
		{URL: "https://h/mcp", Type: "streamable-http", Headers: map[string]string{"A": "2"}},  // header value
		{URL: "https://h/mcp", Type: "streamable-http", Headers: map[string]string{"B": "1"}},  // header key
	} {
		if hashServerConfig(base) == hashServerConfig(mut) {
			t.Fatalf("config change should alter hash: %+v", mut)
		}
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("MCP_TEST_OS", "fromOS")
	env := map[string]string{"TOK": "secret"}
	got := expandEnv("Bearer ${TOK} $MCP_TEST_OS", env)
	if got != "Bearer secret fromOS" {
		t.Fatalf("expandEnv = %q", got)
	}
	// per-server env takes precedence over OS env
	t.Setenv("TOK", "osval")
	if got := expandEnv("${TOK}", env); got != "secret" {
		t.Fatalf("precedence: got %q, want server env 'secret'", got)
	}
}

func TestNewRealChildTransport_HTTP(t *testing.T) {
	// Construction must succeed without connecting (Start() connects later).
	for _, typ := range []string{"", "http", "streamable-http", "sse"} {
		cfg := ServerConfig{URL: "https://example.invalid/mcp", Type: typ}
		if _, err := newRealChildTransport(cfg); err != nil {
			t.Fatalf("type %q: unexpected error %v", typ, err)
		}
	}
	if _, err := newRealChildTransport(ServerConfig{URL: "https://x/mcp", Type: "bogus"}); err == nil {
		t.Fatal("unknown transport type should error")
	}
	if _, err := newRealChildTransport(ServerConfig{Command: "npx", URL: "https://x/mcp"}); err == nil {
		t.Fatal("command+url should error")
	}
}
