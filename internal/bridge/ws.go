package bridge

import (
	"context"
	"net"

	siplib "github.com/emiago/sipgo/sip"
)

// Stub implementations — replaced by full implementation in 06-03.
// These exist so session.go compiles. The real logic is in 06-03.

func dialWS(ctx context.Context, url string) (net.Conn, error) {
	return nil, nil // TODO: 06-03
}

func sendConnected(conn net.Conn) error {
	return nil // TODO: 06-03
}

func sendStart(conn net.Conn, streamSid, callID string, req *siplib.Request) error {
	return nil // TODO: 06-03
}

func sendStop(conn net.Conn, streamSid, callID string) error {
	return nil // TODO: 06-03
}

func writeJSON(conn net.Conn, v any) error {
	return nil // TODO: 06-03
}
