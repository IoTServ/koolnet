package main

import (
	"fmt"
	"net"

	natpmp "github.com/jackpal/go-nat-pmp"
)

func main() {
	gatewayIP := net.IP("10.1.1.1")
	client := natpmp.NewClient(gatewayIP)
	response, err := client.GetExternalAddress()
	if err != nil {
		fmt.Println("err", err)
		return
	}
	fmt.Println("External IP address:", response.ExternalIPAddress)
}
