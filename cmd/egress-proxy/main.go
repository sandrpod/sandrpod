// Copyright 2026 SandrPod Contributors
// sandrpod-egress-proxy — the egress proxy as a standalone process.
//
// This is what a container sandbox talks to. It exists as its own binary (and
// its own small image) because of a constraint that is not obvious until you
// hit it: the poder cannot host the proxy for a container sandbox itself. On
// Linux the poder could bind the docker bridge gateway, but on Docker
// Desktop (macOS/Windows) that address lives inside the Docker VM and the host
// cannot bind it at all. A container joining the sandbox's internal network is
// the one arrangement that works everywhere.
//
// Usage (the poder builds every one of these arguments):
//
//	sandrpod-egress-proxy \
//	  -listen 0.0.0.0:8080 -socks 0.0.0.0:1080 \
//	  -token <per-pod token> \
//	  -policy-json '<egress policy JSON>'
//
// The process serves exactly two things on the network it is attached to, with
// the token as the only credential, and exits when its listeners die. It holds
// no state and writes nothing to disk: the policy arrives on the command line
// and the token never leaves the two containers that need it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/sandrpod/sandrpod/pkg/egress"
)

func main() {
	listen := flag.String("listen", "0.0.0.0:8080", "address for the HTTP proxy")
	socks := flag.String("socks", "0.0.0.0:1080", "address for the SOCKS5 proxy")
	token := flag.String("token", "", "proxy password handed to the sandbox")
	policyJSON := flag.String("policy-json", "", "egress policy as JSON")
	policyFile := flag.String("policy", "", "path to an egress policy file (alternative to -policy-json)")
	flag.Parse()

	if err := run(*listen, *socks, *token, *policyJSON, *policyFile); err != nil {
		fmt.Fprintf(os.Stderr, "sandrpod-egress-proxy: %v\n", err)
		os.Exit(1)
	}
}

func run(listen, socks, token, policyJSON, policyFile string) error {
	if strings.TrimSpace(token) == "" {
		// Without a token anyone on the network can use this proxy, and the
		// network is the sandbox's own. Refuse rather than be an open relay.
		return fmt.Errorf("-token is required")
	}

	var policy egress.Policy
	switch {
	case policyJSON != "" && policyFile != "":
		return fmt.Errorf("give -policy-json or -policy, not both")
	case policyJSON != "":
		dec := json.NewDecoder(strings.NewReader(policyJSON))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&policy); err != nil {
			return fmt.Errorf("decode -policy-json: %w", err)
		}
	case policyFile != "":
		compiled, err := egress.LoadPolicy(policyFile)
		if err != nil {
			return err
		}
		policy = compiled.Policy()
	default:
		// No policy block at all means "deny everything". The poder only starts
		// this process when a sandbox asked for a policy, so reaching here is a
		// bug in the wiring, and the safe reading of a wiring bug is deny all.
		log.Printf("no policy given: denying all egress")
	}

	compiled, err := egress.Parse(policy)
	if err != nil {
		return err
	}

	httpHost, httpPort, err := splitAddr(listen)
	if err != nil {
		return fmt.Errorf("-listen: %w", err)
	}
	socksHost, socksPort, err := splitAddr(socks)
	if err != nil {
		return fmt.Errorf("-socks: %w", err)
	}

	if socksHost != httpHost {
		return fmt.Errorf("-listen and -socks must name the same host (got %q and %q)", httpHost, socksHost)
	}

	srv, err := egress.Start(egress.Options{
		Policy:     compiled,
		Token:      token,
		ListenHost: httpHost,
		HTTPPort:   httpPort,
		SOCKSPort:  socksPort,
		Sink:       logSink{},
	})
	if err != nil {
		return err
	}
	defer srv.Close()

	allow, deny := compiled.Strings()
	log.Printf("egress proxy listening: http=%s socks=%s allow=%v deny=%v",
		srv.HTTPAddr(), srv.SOCKSAddr(), allow, deny)
	// The poder waits for this line in the container log before starting the
	// sandbox, so it must stay exactly one line and must come after Start.

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down")
	return nil
}

// splitAddr parses "host:port" into a bindable host and a fixed port.
func splitAddr(addr string) (string, int, error) {
	i := strings.LastIndexByte(addr, ':')
	if i <= 0 || i == len(addr)-1 {
		return "", 0, fmt.Errorf("%q is not host:port", addr)
	}
	host := addr[:i]
	var port int
	if _, err := fmt.Sscanf(addr[i+1:], "%d", &port); err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("%q has no usable port", addr)
	}
	return host, port, nil
}

// logSink reports refusals on stderr, which is where `docker logs` reads them.
type logSink struct{}

func (logSink) RecordEgress(e egress.Event) {
	log.Printf("egress %s %s:%d code=%s rule=%s reason=%q", e.Kind, e.Host, e.Port, e.Code, e.Rule, e.Reason)
}
