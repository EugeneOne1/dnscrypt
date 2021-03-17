package dnscrypt

import (
	"net"
	"sync"
	"time"

	"golang.org/x/net/context"

	"github.com/AdguardTeam/golibs/log"
	"github.com/miekg/dns"
)

const readTimeout = 2 * time.Second

// ServerDNSCrypt - interface for a DNSCrypt server
type ServerDNSCrypt interface {
	// ServeTCP - listens to TCP connections, queries are then processed by Server.Handler.
	// It blocks the calling goroutine and to stop it you need to close the listener.
	ServeTCP(l net.Listener) error

	// ServeUDP - listens to UDP connections, queries are then processed by Server.Handler.
	// It blocks the calling goroutine and to stop it you need to close the listener.
	ServeUDP(l *net.UDPConn) error

	// Shutdown - tries to gracefully shutdown the server. It waits until all
	// connections are processed and only after that it leaves the method.
	// If context deadline is specified, it will exit earlier.
	Shutdown(ctx context.Context) error
}

// Server - a simple DNSCrypt server implementation
type Server struct {
	// ProviderName - DNSCrypt provider name
	ProviderName string

	// ResolverCert - contains resolver certificate.
	ResolverCert *Cert

	// Handler to invoke. If nil, uses DefaultHandler.
	Handler Handler

	// Shutdown handling
	// --
	lock         sync.RWMutex
	started      bool
	wg           sync.WaitGroup            // active workers (servers)
	tcpListeners map[net.Listener]struct{} // track active TCP listeners
	udpListeners map[*net.UDPConn]struct{} // track active UDP listeners
	tcpConns     map[net.Conn]struct{}     // track active connections
}

// type check
var _ ServerDNSCrypt = &Server{}

// Shutdown - tries to gracefully shutdown the server. It waits until all
// connections are processed and only after that it leaves the method.
// If context deadline is specified, it will exit earlier.
func (s *Server) Shutdown(ctx context.Context) error {
	log.Info("Shutting down the DNSCrypt server")

	s.lock.Lock()
	if !s.started {
		s.lock.Unlock()
		log.Info("Server is not started")
		return ErrServerNotStarted
	}

	s.started = false

	// Unblock reads for all active tcpConns
	for conn := range s.tcpConns {
		_ = conn.SetReadDeadline(time.Unix(1, 0))
	}

	// Unblock reads for all active TCP listeners
	for l := range s.tcpListeners {
		switch v := l.(type) {
		case *net.TCPListener:
			_ = v.SetDeadline(time.Unix(1, 0))
		}
	}

	// Unblock reads for all active UDP listeners
	for l := range s.udpListeners {
		_ = l.SetReadDeadline(time.Unix(1, 0))
	}
	s.lock.Unlock()

	// Using this channel to wait until all goroutines finish their work
	closed := make(chan struct{})
	go func() {
		s.wg.Wait()
		log.Info("Serve goroutines finished their work")
		close(closed)
	}()

	// Wait for either all goroutines finish their work
	// Or for the context deadline
	var ctxErr error
	select {
	case <-closed:
		log.Info("DNSCrypt server has been stopped")
	case <-ctx.Done():
		log.Info("DNSCrypt server shutdown has timed out")
		ctxErr = ctx.Err()
	}

	return ctxErr
}

// init - initializes (lazily) Server properties on startup
// this method is called from Server.ServeTCP and Server.ServeUDP
func (s *Server) init() {
	if s.started && s.tcpConns != nil {
		// The server has already been started, no need to initialize anything
		return
	}
	s.tcpConns = map[net.Conn]struct{}{}
	s.udpListeners = map[*net.UDPConn]struct{}{}
	s.tcpListeners = map[net.Listener]struct{}{}
}

// isStarted - returns true if server is processing queries right now
// it means that Server.ServeTCP or Server.ServeUDP has been called
func (s *Server) isStarted() bool {
	s.lock.RLock()
	started := s.started
	s.lock.RUnlock()
	return started
}

// serveDNS - serves DNS response
func (s *Server) serveDNS(rw ResponseWriter, r *dns.Msg) error {
	if r == nil || len(r.Question) != 1 || r.Response {
		return ErrInvalidQuery
	}

	log.Tracef("Handling a DNS query: %s", r.Question[0].Name)

	handler := s.Handler
	if handler == nil {
		handler = DefaultHandler
	}

	err := handler.ServeDNS(rw, r)
	if err != nil {
		log.Tracef("Error while handing a DNS query: %v", err)

		reply := &dns.Msg{}
		reply.SetRcode(r, dns.RcodeServerFailure)
		_ = rw.WriteMsg(reply)
	}

	return nil
}

// encrypt - encrypts DNSCrypt response
func (s *Server) encrypt(m *dns.Msg, q EncryptedQuery) ([]byte, error) {
	r := EncryptedResponse{
		EsVersion: q.EsVersion,
		Nonce:     q.Nonce,
	}
	packet, err := m.Pack()
	if err != nil {
		return nil, err
	}

	sharedKey, err := computeSharedKey(q.EsVersion, &s.ResolverCert.ResolverSk, &q.ClientPk)
	if err != nil {
		return nil, err
	}

	return r.Encrypt(packet, sharedKey)
}

// decrypt - decrypts the incoming message and returns a DNS message to process
func (s *Server) decrypt(b []byte) (*dns.Msg, EncryptedQuery, error) {
	q := EncryptedQuery{
		EsVersion:   s.ResolverCert.EsVersion,
		ClientMagic: s.ResolverCert.ClientMagic,
	}
	msg, err := q.Decrypt(b, s.ResolverCert.ResolverSk)
	if err != nil {
		// Failed to decrypt, dropping it
		return nil, q, err
	}

	r := new(dns.Msg)
	err = r.Unpack(msg)
	if err != nil {
		// Invalid DNS message, ignore
		return nil, q, err
	}

	return r, q, nil
}

// handleHandshake - handles a TXT request that requests certificate data
func (s *Server) handleHandshake(b []byte, certTxt string) ([]byte, error) {
	m := new(dns.Msg)
	err := m.Unpack(b)
	if err != nil {
		// Not a handshake, just ignore it
		return nil, err
	}

	if len(m.Question) != 1 || m.Response {
		// Invalid query
		return nil, ErrInvalidQuery
	}

	q := m.Question[0]
	providerName := dns.Fqdn(s.ProviderName)
	if q.Qtype != dns.TypeTXT || q.Name != providerName {
		// Invalid provider name or type, doing nothing
		return nil, ErrInvalidQuery
	}

	reply := new(dns.Msg)
	reply.SetReply(m)
	txt := &dns.TXT{
		Hdr: dns.RR_Header{
			Name:   q.Name,
			Rrtype: dns.TypeTXT,
			Ttl:    60, // use 60 seconds by default, but it shouldn't matter
			Class:  dns.ClassINET,
		},
		Txt: []string{
			certTxt,
		},
	}
	reply.Answer = append(reply.Answer, txt)
	return reply.Pack()
}

// validate - checks if the Server config is properly set
func (s *Server) validate() bool {
	if s.ResolverCert == nil {
		log.Error("ResolverCert must be set")
		return false
	}

	if !s.ResolverCert.VerifyDate() {
		log.Error("ResolverCert date is not valid")
		return false
	}

	if s.ProviderName == "" {
		log.Error("ProviderName must be set")
		return false
	}

	return true
}
