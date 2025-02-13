// Command wgctrl is a testing utility for interacting with WireGuard via package
// wgctrl.
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"strings"

	"github.com/danpashin/wgctrl"
	"github.com/danpashin/wgctrl/wgtypes"
)

func main() {
	flag.Parse()

	clientTypes := [](wgtypes.ClientType){
		wgtypes.NativeClient, wgtypes.AmneziaClient,
	}

	for _, clientType := range clientTypes {
		c, err := wgctrl.New(clientType)
		if err != nil {
			log.Fatalf("failed to open wgctrl: %v", err)
		}
		defer c.Close()

		var devices []*wgtypes.Device
		if device := flag.Arg(0); device != "" {
			d, err := c.Device(device)
			if err != nil {
				log.Fatalf("failed to get device %q: %v", device, err)
			}

			devices = append(devices, d)
		} else {
			devices, err = c.Devices()
			if err != nil {
				log.Fatalf("failed to get devices: %v", err)
			}
		}

		for _, d := range devices {
			printDevice(d)

			for _, p := range d.Peers {
				printPeer(p)
			}
		}
	}
}

func printDevice(d *wgtypes.Device) {
	const f = `interface: %s (%s)
  public key: %s
  private key: (hidden)
  listening port: %d

`
	const advancedSecF = `  JC: %d
  JMin: %d
  JMax: %d
  S1: %d
  S2: %d
  H1: %d
  H2: %d
  H3: %d
  H4: %d

`

	fmt.Printf(
		f,
		d.Name,
		d.Type.String(),
		d.PublicKey.String(),
		d.ListenPort)

	if d.AdvancedSecurity.IsEnabled() {
		fmt.Printf(
			advancedSecF,
			d.AdvancedSecurity.JunkPacketCount,
			d.AdvancedSecurity.JunkPacketMinSize,
			d.AdvancedSecurity.JunkPacketMaxSize,
			d.AdvancedSecurity.InitPacketJunkSize,
			d.AdvancedSecurity.ResponsePacketJunkSize,
			d.AdvancedSecurity.InitPacketMagicHeader,
			d.AdvancedSecurity.ResponsePacketMagicHeader,
			d.AdvancedSecurity.UnderloadPacketMagicHeader,
			d.AdvancedSecurity.TransportPacketMagicHeader,
		)
	}
}

func printPeer(p wgtypes.Peer) {
	const f = `peer: %s
  endpoint: %s
  allowed ips: %s
  latest handshake: %s
  transfer: %d B received, %d B sent

`

	fmt.Printf(
		f,
		p.PublicKey.String(),
		// TODO(mdlayher): get right endpoint with getnameinfo.
		p.Endpoint.String(),
		ipsString(p.AllowedIPs),
		p.LastHandshakeTime.String(),
		p.ReceiveBytes,
		p.TransmitBytes,
	)
}

func ipsString(ipns []net.IPNet) string {
	ss := make([]string, 0, len(ipns))
	for _, ipn := range ipns {
		ss = append(ss, ipn.String())
	}

	return strings.Join(ss, ", ")
}
