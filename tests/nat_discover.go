package main

import (
	"fmt"
	"github.com/ccding/go-stun/stun"
)

func main() {
	c := stun.NewClient()
	c.SetServerHost("stun.ekiga.net", 3478)
	c.SetSoftwareName("koolnet")
	nat, host, err := c.Discover()
	if err != nil {
		fmt.Println(err)
	}

	switch nat {
	case stun.NAT_ERROR:
		fmt.Println("Test failed")
	case stun.NAT_UNKNOWN:
		fmt.Println("Unexpected response from the STUN server")
	case stun.NAT_BLOCKED:
		fmt.Println("UDP is blocked")
	case stun.NAT_FULL:
		fmt.Println("Full cone NAT")
	case stun.NAT_SYMETRIC:
		fmt.Println("Symetric NAT")
	case stun.NAT_RESTRICTED:
		fmt.Println("Restricted NAT")
	case stun.NAT_PORT_RESTRICTED:
		fmt.Println("Port restricted NAT")
	case stun.NAT_NONE:
		fmt.Println("Not behind a NAT")
	case stun.NAT_SYMETRIC_UDP_FIREWALL:
		fmt.Println("Symetric UDP firewall")
	}

	if host != nil {
		fmt.Println(host.Family())
		fmt.Println(host.IP())
		fmt.Println(host.Port())
	}
}
