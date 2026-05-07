// Minimal SIP REGISTER responder stub for sipp-driven E2E scenarios.
//
// The bridge under test (cmd/sipgate-sip-stream-bridge) requires a successful
// REGISTER → 200 OK round-trip at startup (main.go fatals otherwise).
// This stub binds UDP:5060 (or $REG_LISTEN_ADDR) and responds 200 OK to any
// REGISTER, echoing the supplied Contact and Expires.
//
// Scope: just enough to let main.go progress past registrar.Register() so
// inbound INVITEs (the actual scenarios `a`-`h` exercise) can be processed.
// Does NOT implement digest auth (the bridge will skip the challenge cycle
// when the stub returns 200 immediately on the first REGISTER).
//
// NOT a production artifact. Only loaded by tests/e2e/sipp/run-sipp.sh.

package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:5060", "UDP address to bind for REGISTER")
	flag.Parse()

	if env := os.Getenv("REG_LISTEN_ADDR"); env != "" {
		*addr = env
	}

	udpAddr, err := net.ResolveUDPAddr("udp", *addr)
	if err != nil {
		log.Fatalf("resolve %s: %v", *addr, err)
	}
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", *addr, err)
	}
	defer conn.Close()

	log.Printf("test-registrar stub listening on udp://%s (REGISTER → 200 OK)", *addr)

	// Graceful shutdown on signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		_ = conn.Close()
	}()

	buf := make([]byte, 65535)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if strings.Contains(err.Error(), "use of closed") {
				return
			}
			log.Printf("read: %v", err)
			continue
		}
		msg := string(buf[:n])
		if !strings.HasPrefix(msg, "REGISTER ") {
			// Non-REGISTER UDP traffic — ignore. Bridge sends OPTIONS keepalive,
			// the test-registrar stub only handles REGISTER.
			continue
		}
		resp := build200(msg)
		if _, err := conn.WriteToUDP([]byte(resp), src); err != nil {
			log.Printf("write to %s: %v", src, err)
		}
	}
}

// build200 echoes Via, From, To, Call-ID, CSeq, Contact (if present) and
// emits a 200 OK with Expires:120. The bridge's sipgo client does the rest.
func build200(req string) string {
	lines := strings.Split(req, "\r\n")

	var via, from, to, callID, cseq, contact, expires string
	for _, l := range lines {
		lower := strings.ToLower(l)
		switch {
		case strings.HasPrefix(lower, "via:"):
			if via == "" {
				via = l
			}
		case strings.HasPrefix(lower, "from:"):
			from = l
		case strings.HasPrefix(lower, "to:"):
			to = l
		case strings.HasPrefix(lower, "call-id:"):
			callID = l
		case strings.HasPrefix(lower, "cseq:"):
			cseq = l
		case strings.HasPrefix(lower, "contact:"):
			contact = l
		case strings.HasPrefix(lower, "expires:"):
			expires = l
		}
	}
	if expires == "" {
		expires = "Expires: 120"
	}
	if contact == "" {
		// Synthesize a contact line if the REGISTER omits it (defensive).
		contact = "Contact: <sip:stub@127.0.0.1:5060>"
	}

	headers := []string{
		"SIP/2.0 200 OK",
		via,
		from,
		to + ";tag=stub-registrar",
		callID,
		cseq,
		contact,
		expires,
		"Server: audio-dock-test-registrar/0.1",
		"Content-Length: 0",
		"",
		"",
	}
	return fmt.Sprintf("%s", strings.Join(headers, "\r\n"))
}
