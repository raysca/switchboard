package netpool

import (
	"crypto/tls"
	"crypto/x509"
	"net"
)

type netConn = net.Conn

func clientTLS(s *srv) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(s.Certificate())
	return &tls.Config{RootCAs: pool}
}
