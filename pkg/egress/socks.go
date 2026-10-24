// Copyright 2026 SandrPod
// The SOCKS5 half of the egress proxy.
//
// HTTP proxies only cover tools that speak HTTP. ssh, git-over-ssh, database
// clients and anything else that opens a raw TCP connection need SOCKS5, and
// a sandbox whose egress is "HTTP only" is a sandbox with a hole in it.
//
// Only CONNECT is implemented (RFC 1928 command 0x01): BIND and UDP ASSOCIATE
// are refused rather than half-supported, because a UDP relay would need its
// own allow-list semantics and a BIND would let a sandbox accept inbound
// connections, which is the opposite of egress control.
//
// Username/password auth (RFC 1929) is required and the password is the same
// per-server token the HTTP side takes. Without it any local process could
// borrow the sandbox's allow-list — the proxy binds to loopback, so "local"
// means "anything else running as this user", which is exactly the population
// that must not get a free pass.
package egress

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

const (
	socksVersion = 0x05

	socksMethodUserPass = 0x02
	socksMethodNone     = 0x00

	socksCmdConnect = 0x01

	socksAtypIPv4   = 0x01
	socksAtypDomain = 0x03
	socksAtypIPv6   = 0x04

	socksRepSuccess       = 0x00
	socksRepGeneralFail   = 0x01
	socksRepNotAllowed    = 0x02
	socksRepHostUnreach   = 0x04
	socksRepConnRefused   = 0x05
	socksRepCmdNotSupport = 0x07
	socksRepAtypNotSupp   = 0x08

	socksAuthVersion = 0x01
	socksAuthOK      = 0x00
	socksAuthFail    = 0x01
)

// acceptSOCKS accepts connections until the listener closes.
func (s *Server) acceptSOCKS() {
	for {
		conn, err := s.socksLn.Accept()
		if err != nil {
			return // listener closed
		}
		go s.handleSOCKS(conn)
	}
}

// handleSOCKS runs the RFC 1928 handshake for one connection.
func (s *Server) handleSOCKS(conn net.Conn) {
	defer conn.Close()

	// A hung handshake must not pin a goroutine forever; the deadline is
	// dropped once the tunnel is up.
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	br := bufio.NewReader(conn)

	if err := s.socksNegotiate(conn, br); err != nil {
		return
	}

	host, port, err := socksReadRequest(br)
	if err != nil {
		switch {
		case errors.Is(err, errCmdNotSupported):
			socksReply(conn, socksRepCmdNotSupport)
		case errors.Is(err, errAtypNotSupported):
			socksReply(conn, socksRepAtypNotSupp)
		}
		return
	}

	verdict := s.opts.Policy.Decide(host, port)
	if !verdict.Allow {
		s.record(Event{Kind: CodeSOCKS, Host: verdict.Host, Port: verdict.Port,
			Code: verdict.Code, Rule: verdict.Rule, Reason: verdict.Reason, Method: "CONNECT"})
		socksReply(conn, socksRepNotAllowed)
		return
	}

	upstream, derr := s.dialer.dialAllowingVetted(context.Background(), host, port)
	if derr != nil {
		if deny, ok := asDenyError(derr); ok {
			s.record(Event{Kind: CodeSOCKS, Host: host, Port: port, Code: deny.Verdict.Code,
				Reason: deny.Verdict.Reason, Address: deny.Verdict.Address, Method: "CONNECT"})
			socksReply(conn, socksRepNotAllowed)
			return
		}
		rep := byte(socksRepHostUnreach)
		if strings.Contains(strings.ToLower(derr.Error()), "refused") {
			rep = socksRepConnRefused
		}
		socksReply(conn, rep)
		return
	}
	defer upstream.Close()
	s.record(Event{Kind: CodeSOCKS, Host: host, Port: port, Code: CodeAllowed,
		Rule: verdict.Rule, Method: "CONNECT"})

	_ = conn.SetDeadline(time.Time{})
	if err := socksReplyOK(conn); err != nil {
		return
	}
	s.splice(conn, upstream)
}

// socksNegotiate picks the auth method and runs the RFC 1929 exchange.
func (s *Server) socksNegotiate(conn net.Conn, br *bufio.Reader) error {
	head := make([]byte, 2)
	if _, err := io.ReadFull(br, head); err != nil {
		return err
	}
	if head[0] != socksVersion {
		return fmt.Errorf("socks: unsupported version %d", head[0])
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(br, methods); err != nil {
		return err
	}
	if !containsByte(methods, socksMethodUserPass) {
		// Do not accept no-auth: the token is the only thing keeping other
		// local processes out of the sandbox's allow-list.
		_, _ = conn.Write([]byte{socksVersion, 0xFF})
		return fmt.Errorf("socks: client offered no username/password method")
	}
	if _, err := conn.Write([]byte{socksVersion, socksMethodUserPass}); err != nil {
		return err
	}

	// RFC 1929: VER, ULEN, UNAME, PLEN, PASSWD.
	var b [2]byte
	if _, err := io.ReadFull(br, b[:]); err != nil {
		return err
	}
	if b[0] != socksAuthVersion {
		return fmt.Errorf("socks: unsupported auth version %d", b[0])
	}
	user := make([]byte, int(b[1]))
	if _, err := io.ReadFull(br, user); err != nil {
		return err
	}
	if _, err := io.ReadFull(br, b[:1]); err != nil {
		return err
	}
	pass := make([]byte, int(b[0]))
	if _, err := io.ReadFull(br, pass); err != nil {
		return err
	}
	if !s.tokenMatches(string(pass)) {
		_, _ = conn.Write([]byte{socksAuthVersion, socksAuthFail})
		return fmt.Errorf("socks: bad token")
	}
	_, err := conn.Write([]byte{socksAuthVersion, socksAuthOK})
	return err
}

// socksReadRequest parses a CONNECT request: VER, CMD, RSV, ATYP, DST.ADDR,
// DST.PORT.
func socksReadRequest(br *bufio.Reader) (string, int, error) {
	var head [4]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		return "", 0, err
	}
	if head[0] != socksVersion {
		return "", 0, fmt.Errorf("socks: unsupported version %d", head[0])
	}
	if head[1] != socksCmdConnect {
		// Caller answers 0x07 itself; signal it through the error text so a
		// single code path writes the reply.
		return "", 0, errCmdNotSupported
	}

	var host string
	switch head[3] {
	case socksAtypIPv4:
		var v [4]byte
		if _, err := io.ReadFull(br, v[:]); err != nil {
			return "", 0, err
		}
		host = net.IP(v[:]).String()
	case socksAtypIPv6:
		var v [16]byte
		if _, err := io.ReadFull(br, v[:]); err != nil {
			return "", 0, err
		}
		host = net.IP(v[:]).String()
	case socksAtypDomain:
		n, err := br.ReadByte()
		if err != nil {
			return "", 0, err
		}
		name := make([]byte, int(n))
		if _, err := io.ReadFull(br, name); err != nil {
			return "", 0, err
		}
		host = string(name)
	default:
		return "", 0, errAtypNotSupported
	}

	var portBuf [2]byte
	if _, err := io.ReadFull(br, portBuf[:]); err != nil {
		return "", 0, err
	}
	return host, int(binary.BigEndian.Uint16(portBuf[:])), nil
}

// socksReplyOK writes a success reply with an all-zero bound address, which
// every client tolerates and no client needs when the target is CONNECT.
func socksReplyOK(conn net.Conn) error {
	_, err := conn.Write([]byte{socksVersion, socksRepSuccess, 0x00, socksAtypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

func socksReply(conn net.Conn, rep byte) {
	_, _ = conn.Write([]byte{socksVersion, rep, 0x00, socksAtypIPv4, 0, 0, 0, 0, 0, 0})
}

func containsByte(haystack []byte, needle byte) bool {
	for _, b := range haystack {
		if b == needle {
			return true
		}
	}
	return false
}

var (
	errCmdNotSupported  = fmt.Errorf("socks: command not supported")
	errAtypNotSupported = fmt.Errorf("socks: address type not supported")
)
