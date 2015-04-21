package mdns

import (
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/hashicorp/go.net/ipv4"
	"github.com/hashicorp/go.net/ipv6"
	"github.com/miekg/dns"
)

const (
	ipv4mdns              = "224.0.0.251"
	ipv6mdns              = "ff02::fb"
	mdnsPort              = 5353
	forceUnicastResponses = false
)

var (
	ipv4Addr = &net.UDPAddr{
		IP:   net.ParseIP(ipv4mdns),
		Port: mdnsPort,
	}
	ipv6Addr = &net.UDPAddr{
		IP:   net.ParseIP(ipv6mdns),
		Port: mdnsPort,
	}
)

// Config is used to configure the mDNS server
type Config struct {
	// Zone must be provided to support responding to queries
	Zone Zone

	// Iface if provided binds the multicast listener to the given
	// interface. If not provided, the system default multicase interface
	// is used.
	Iface *net.Interface

	// Whether to set the IP_MULTICAST_LOOP socket option on the multicast sockets
	// opened.  Setting this to true allows mDNS clients on the same machine to
	// discover the service. See
	// http://stackoverflow.com/questions/1719156/is-there-a-way-to-test-multicast-ip-on-same-box
	DisableMulticastLoopback bool
}

// mDNS server is used to listen for mDNS queries and respond if we
// have a matching local record
type Server struct {
	config *Config

	ipv4List *net.UDPConn
	ipv6List *net.UDPConn

	shutdown     bool
	shutdownCh   chan struct{}
	shutdownLock sync.Mutex
}

// NewServer is used to create a new mDNS server from a config
func NewServer(config *Config) (*Server, error) {
	// Create the listeners
	// TODO(reddaly): Handle errors returned by ListenMulticastUDP
	ipv4List, _ := net.ListenMulticastUDP("udp4", config.Iface, ipv4Addr)
	ipv6List, _ := net.ListenMulticastUDP("udp6", config.Iface, ipv6Addr)

	{
		p := ipv4.NewPacketConn(ipv4List)
		if err := p.SetMulticastLoopback(!config.DisableMulticastLoopback); err != nil {
			return nil, err
		}
	}
	{
		p := ipv6.NewPacketConn(ipv6List)
		if err := p.SetMulticastLoopback(!config.DisableMulticastLoopback); err != nil {
			return nil, err
		}
	}

	// Check if we have any listener
	if ipv4List == nil && ipv6List == nil {
		return nil, fmt.Errorf("No multicast listeners could be started")
	}

	s := &Server{
		config:     config,
		ipv4List:   ipv4List,
		ipv6List:   ipv6List,
		shutdownCh: make(chan struct{}),
	}

	if ipv4List != nil {
		go s.recv(s.ipv4List)
	}

	if ipv6List != nil {
		go s.recv(s.ipv6List)
	}

	s.Announce()
	return s, nil
}

// Shutdown is used to shutdown the listener
func (s *Server) Shutdown() error {
	s.shutdownLock.Lock()
	defer s.shutdownLock.Unlock()

	if s.shutdown {
		return nil
	}
	s.shutdown = true
	close(s.shutdownCh)

	if s.ipv4List != nil {
		s.ipv4List.Close()
	}
	if s.ipv6List != nil {
		s.ipv6List.Close()
	}
	return nil
}

// recv is a long running routine to receive packets from an interface
func (s *Server) recv(c *net.UDPConn) {
	if c == nil {
		return
	}
	buf := make([]byte, 65536)
	for !s.shutdown {
		n, from, err := c.ReadFrom(buf)
		if err != nil {
			continue
		}
		if err := s.parsePacket(buf[:n], from); err != nil {
			log.Printf("[ERR] mdns: Failed to handle query: %v", err)
		}
	}
}

// parsePacket is used to parse an incoming packet
func (s *Server) parsePacket(packet []byte, from net.Addr) error {
	var msg dns.Msg
	if err := msg.Unpack(packet); err != nil {
		log.Printf("[ERR] mdns: Failed to unpack packet: %v", err)
		return err
	}
	return s.handleQuery(&msg, from)
}

// handleQuery is used to handle an incoming query
func (s *Server) handleQuery(query *dns.Msg, from net.Addr) error {
	if query.Opcode != dns.OpcodeQuery {
		// "In both multicast query and multicast response messages, the OPCODE MUST
		// be zero on transmission (only standard queries are currently supported
		// over multicast).  Multicast DNS messages received with an OPCODE other
		// than zero MUST be silently ignored."  Note: OpcodeQuery == 0
		return fmt.Errorf("mdns: received query with non-zero Opcode %v: %v", query.Opcode, *query)
	}
	if query.Rcode != 0 {
		// "In both multicast query and multicast response messages, the Response
		// Code MUST be zero on transmission.  Multicast DNS messages received with
		// non-zero Response Codes MUST be silently ignored."
		return fmt.Errorf("mdns: received query with non-zero Rcode %v: %v", query.Rcode, *query)
	}

	// TODO(reddaly): Handle "TC (Truncated) Bit":
	//    In query messages, if the TC bit is set, it means that additional
	//    Known-Answer records may be following shortly.  A responder SHOULD
	//    record this fact, and wait for those additional Known-Answer records,
	//    before deciding whether to respond.  If the TC bit is clear, it means
	//    that the querying host has no additional Known Answers.
	if query.Truncated {
		return fmt.Errorf("[ERR] mdns: support for DNS requests with high truncated bit not implemented: %v", *query)
	}

	var unicastAnswer, multicastAnswer []dns.RR

	// Handle each question
	for _, q := range query.Question {
		mrecs, urecs := s.handleQuestion(q)
		multicastAnswer = append(multicastAnswer, mrecs...)
		unicastAnswer = append(unicastAnswer, urecs...)
	}

	// See section 18 of RFC 6762 for rules about DNS headers.
	resp := func(unicast bool) *dns.Msg {
		// 18.1: ID (Query Identifier)
		// 0 for multicast response, query.Id for unicast response
		id := uint16(0)
		if unicast {
			id = query.Id
		}

		var answer []dns.RR
		if unicast {
			answer = unicastAnswer
		} else {
			answer = multicastAnswer
		}
		if len(answer) == 0 {
			return nil
		}

		return &dns.Msg{
			MsgHdr: dns.MsgHdr{
				Id: id,

				// 18.2: QR (Query/Response) Bit - must be set to 1 in response.
				Response: true,

				// 18.3: OPCODE - must be zero in response (OpcodeQuery == 0)
				Opcode: dns.OpcodeQuery,

				// 18.4: AA (Authoritative Answer) Bit - must be set to 1
				Authoritative: true,

				// The following fields must all be set to 0:
				// 18.5: TC (TRUNCATED) Bit
				// 18.6: RD (Recursion Desired) Bit
				// 18.7: RA (Recursion Available) Bit
				// 18.8: Z (Zero) Bit
				// 18.9: AD (Authentic Data) Bit
				// 18.10: CD (Checking Disabled) Bit
				// 18.11: RCODE (Response Code)
			},
			// 18.12 pertains to questions (handled by handleQuestion)
			// 18.13 pertains to resource records (handled by handleQuestion)

			// 18.14: Name Compression - responses should be compressed (though see
			// caveats in the RFC), so set the Compress bit (part of the dns library
			// API, not part of the DNS packet) to true.
			Compress: true,

			Answer: answer,
		}
	}

	if mresp := resp(false); mresp != nil {
		if err := s.sendResponse(mresp, from, false); err != nil {
			return fmt.Errorf("mdns: error sending multicast response: %v", err)
		}
	}
	if uresp := resp(true); uresp != nil {
		if err := s.sendResponse(uresp, from, true); err != nil {
			return fmt.Errorf("mdns: error sending unicast response: %v", err)
		}
	}
	return nil
}

// handleQuestion is used to handle an incoming question
//
// The response to a question may be transmitted over multicast, unicast, or
// both.  The return values are DNS records for each transmission type.
func (s *Server) handleQuestion(q dns.Question) (multicastRecs, unicastRecs []dns.RR) {
	records := s.config.Zone.Records(q)

	if len(records) == 0 {
		return nil, nil
	}

	// Handle unicast and multicast responses.
	// TODO(reddaly): The decision about sending over unicast vs. multicast is not
	// yet fully compliant with RFC 6762.  For example, the unicast bit should be
	// ignored if the records in question are close to TTL expiration.  For now,
	// we just use the unicast bit to make the decision, as per the spec:
	//     RFC 6762, section 18.12.  Repurposing of Top Bit of qclass in Question
	//     Section
	//
	//     In the Question Section of a Multicast DNS query, the top bit of the
	//     qclass field is used to indicate that unicast responses are preferred
	//     for this particular question.  (See Section 5.4.)
	if q.Qclass&(1<<15) != 0 || forceUnicastResponses {
		return nil, records
	}
	return records, nil
}

// sendResponse is used to send a response packet
func (s *Server) sendResponse(resp *dns.Msg, from net.Addr, unicast bool) error {
	buf, err := resp.Pack()
	if err != nil {
		return err
	}

	// Determine the socket to send from
	addr := from.(*net.UDPAddr)
	var conn *net.UDPConn
	if addr.IP.To4() != nil {
		conn = s.ipv4List
	} else {
		conn = s.ipv6List
	}

	// Determine the address to send to if not unicast
	if !unicast {
		if addr.IP.To4() != nil {
			addr = ipv4Addr
		} else {
			addr = ipv6Addr
		}
	}

	// Guard against a missing connection or address
	if conn == nil || addr == nil {
		return fmt.Errorf("Unable to respond, missing connection (%v) or address (%v)",
			conn, addr)
	}
	_, err = conn.WriteToUDP(buf, addr)
	return err
}

// Probe requests DNS records on the network that may conflict with those of
// this service, as per section 8.1 (Probing) of RFC6762.
func (s *Server) Probe() {
	// TODO(reddaly): Implement me.
}

func (s *Server) announceOnce() error {
	records := s.config.Zone.Announcement()
	if len(records) == 0 {
		return nil
	}

	msg := &dns.Msg{
		MsgHdr: dns.MsgHdr{
			// 18.1: ID (Query Identifier) - 0 for multicast
			Id: 0,

			// 18.2: QR (Query/Response) Bit - must be set to 1 in response.
			Response: true,

			// 18.3: OPCODE - must be zero in response (OpcodeQuery == 0)
			Opcode: dns.OpcodeQuery,

			// 18.4: AA (Authoritative Answer) Bit - must be set to 1
			Authoritative: true,

			// The following fields must all be set to 0:
			// 18.5: TC (TRUNCATED) Bit
			// 18.6: RD (Recursion Desired) Bit
			// 18.7: RA (Recursion Available) Bit
			// 18.8: Z (Zero) Bit
			// 18.9: AD (Authentic Data) Bit
			// 18.10: CD (Checking Disabled) Bit
			// 18.11: RCODE (Response Code)
		},
		// 18.12 pertains to questions (handled by handleQuestion)
		// 18.13 pertains to resource records (handled by handleQuestion)

		// 18.14: Name Compression - responses should be compressed (though see
		// caveats in the RFC), so set the Compress bit (part of the dns library
		// API, not part of the DNS packet) to true.
		Compress: true,

		Answer: records,
	}

	buf, err := msg.Pack()
	if err != nil {
		return err
	}

	if s.ipv4List != nil {
		if _, err = s.ipv4List.WriteToUDP(buf, ipv4Addr); err != nil {
			return err
		}
	}
	if s.ipv6List != nil {
		if _, err = s.ipv6List.WriteToUDP(buf, ipv6Addr); err != nil {
			return err
		}
	}
	return nil
}

// Announce broadcasts DNS records to the network that describe this service, as
// per section 8.3 (Announcing) of RFC6762.
func (s *Server) Announce() {
	// The Multicast DNS responder MUST send at least two unsolicited responses,
	// one second apart.  To provide increased robustness against packet loss, a
	// responder MAY send up to eight unsolicited responses, provided that the
	// interval between unsolicited responses increases by at least a factor of
	// two with every response sent.

	var timer *time.Timer
	tryAnnounce := func() {
		if err := s.announceOnce(); err != nil {
			timer.Stop()
			log.Printf("[ERR] mdns: Announce failed: %v", err)
		}
	}
	timer = time.AfterFunc(time.Second*2, tryAnnounce)
	go tryAnnounce()
}
