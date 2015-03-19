// Program server is an example mDNS server.
package main

import (
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"

	"github.com/gonzojive/mdns"
)

func main() {
	// Setup our service export
	txt := []string{"txtvers=1"}
	host, err := os.Hostname()
	if err != nil {
		log.Fatalf("Failed to get hostname: %v", err)
	}
	host += ".local."

	ips := []net.IP{}
	service, err := mdns.NewMDNSService(fmt.Sprintf("hello world %d", rand.Uint32()%9999), "_testing._tcp", "local.", host, 4242, ips, txt)
	if err != nil {
		log.Fatalf("error creating mDNS service: %v", err)
	}

	// Create the mDNS server, defer shutdown
	server, _ := mdns.NewServer(&mdns.Config{Zone: service})
	defer server.Shutdown()
	log.Printf("Started mDNS server")
	select {}
}
