//+build linux

package wgnl

import (
	"fmt"
	"net"
	"os"
	"time"
	"unsafe"

	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/netlink"
	"github.com/mdlayher/netlink/nlenc"
	"github.com/mdlayher/wireguardctrl/internal/wgnl/internal/wgh"
	"github.com/mdlayher/wireguardctrl/wgtypes"
	"golang.org/x/sys/unix"
)

var _ osClient = &client{}

// A client is a Linux-specific wireguard netlink client.
type client struct {
	c      *genetlink.Conn
	family genetlink.Family

	interfaces func() ([]net.Interface, error)
}

// newClient opens a connection to the wireguard family using generic netlink.
func newClient() (*client, error) {
	c, err := genetlink.Dial(nil)
	if err != nil {
		return nil, err
	}

	return initClient(c)
}

// initClient is the internal client constructor used in some tests.
func initClient(c *genetlink.Conn) (*client, error) {
	f, err := c.GetFamily(wgh.GenlName)
	if err != nil {
		_ = c.Close()
		return nil, err
	}

	return &client{
		c:      c,
		family: f,

		// By default, gather interfaces using package net.
		interfaces: net.Interfaces,
	}, nil
}

// Close implements osClient.
func (c *client) Close() error {
	return c.c.Close()
}

// Devices implements osClient.
func (c *client) Devices() ([]*wgtypes.Device, error) {
	// TODO(mdlayher): it doesn't seem possible to do a typical netlink dump
	// of all WireGuard devices.  Perhaps consider raising this to the developers
	// to solicit their feedback.
	ifis, err := c.interfaces()
	if err != nil {
		return nil, err
	}

	var ds []*wgtypes.Device
	for _, ifi := range ifis {
		// Attempt to fetch device information.  If we receive a "not exist"
		// error, the device must not be a WireGuard device.
		d, err := c.getDevice(ifi.Index, ifi.Name)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}

			return nil, err
		}

		ds = append(ds, d)
	}

	return ds, nil
}

// DeviceByIndex implements osClient.
func (c *client) DeviceByIndex(index int) (*wgtypes.Device, error) {
	return c.getDevice(index, "")
}

// DeviceByName implements osClient.
func (c *client) DeviceByName(name string) (*wgtypes.Device, error) {
	return c.getDevice(0, name)
}

// ConfigureDevice implements osClient.
func (c *client) ConfigureDevice(name string, cfg wgtypes.Config) error {
	attrs, err := configAttrs(name, cfg)
	if err != nil {
		return err
	}

	// Request acknowledgement of our request from netlink, even though the
	// output messages are unused.  The netlink package checks and trims the
	// status code value.
	flags := netlink.HeaderFlagsRequest | netlink.HeaderFlagsAcknowledge
	if _, err := c.execute(wgh.CmdSetDevice, flags, attrs); err != nil {
		return err
	}

	return nil
}

// getDevice fetches a Device using either its index or name, depending on which
// is specified.  If both are specified, index is preferred.
func (c *client) getDevice(index int, name string) (*wgtypes.Device, error) {
	// WireGuard netlink expects either interface index or name for all queries.
	var attr netlink.Attribute
	switch {
	case index != 0:
		attr = netlink.Attribute{
			Type: wgh.DeviceAIfindex,
			Data: nlenc.Uint32Bytes(uint32(index)),
		}
	case name != "":
		attr = netlink.Attribute{
			Type: wgh.DeviceAIfname,
			Data: nlenc.Bytes(name),
		}
	default:
		// No information provided, nothing to do.
		return nil, os.ErrNotExist
	}

	flags := netlink.HeaderFlagsRequest | netlink.HeaderFlagsDump
	msgs, err := c.execute(wgh.CmdGetDevice, flags, []netlink.Attribute{attr})
	if err != nil {
		return nil, err
	}

	return parseDevice(msgs)
}

// execute executes a single WireGuard netlink request with the specified command,
// header flags, and attribute arguments.
func (c *client) execute(command uint8, flags netlink.HeaderFlags, attrs []netlink.Attribute) ([]genetlink.Message, error) {
	b, err := netlink.MarshalAttributes(attrs)
	if err != nil {
		return nil, err
	}

	msg := genetlink.Message{
		Header: genetlink.Header{
			Command: command,
			Version: wgh.GenlVersion,
		},
		Data: b,
	}

	msgs, err := c.c.Execute(msg, c.family.ID, flags)
	if err != nil {
		switch err {
		// Convert "no such device" and "not a wireguard device" to an error
		// compatible with os.IsNotExist for easy checking.
		case unix.ENODEV, unix.ENOTSUP:
			return nil, os.ErrNotExist
		default:
			return nil, err
		}
	}

	return msgs, nil
}

// configAttrs creates the required netlink attributes to configure the device
// specified by name using the non-nil fields in cfg.
func configAttrs(name string, cfg wgtypes.Config) ([]netlink.Attribute, error) {
	attrs := []netlink.Attribute{{
		Type: wgh.DeviceAIfname,
		Data: nlenc.Bytes(name),
	}}

	if cfg.PrivateKey != nil {
		attrs = append(attrs, netlink.Attribute{
			Type: wgh.DeviceAPrivateKey,
			Data: (*cfg.PrivateKey)[:],
		})
	}

	return attrs, nil
}

// parseDevice parses a Device from a slice of generic netlink messages,
// automatically merging peer lists from subsequent messages into the Device
// from the first message.
func parseDevice(msgs []genetlink.Message) (*wgtypes.Device, error) {
	var first wgtypes.Device
	for i, m := range msgs {
		d, err := parseDeviceLoop(m)
		if err != nil {
			return nil, err
		}

		if i == 0 {
			// First message contains our target device.
			first = *d
			continue
		}

		// Any subsequent messages have their peer contents merged into the
		// first "target" message.
		if err := mergeDevices(&first, d); err != nil {
			return nil, err
		}
	}

	return &first, nil
}

// parseDeviceLoop parses a Device from a single generic netlink message.
func parseDeviceLoop(m genetlink.Message) (*wgtypes.Device, error) {
	ad, err := netlink.NewAttributeDecoder(m.Data)
	if err != nil {
		return nil, err
	}

	var d wgtypes.Device
	for ad.Next() {
		switch ad.Type() {
		case wgh.DeviceAIfindex:
			// Ignored; interface index isn't exposed at all in the userspace
			// configuration protocol, and name is more friendly anyway.
		case wgh.DeviceAIfname:
			d.Name = ad.String()
		case wgh.DeviceAPrivateKey:
			ad.Do(parseKey(&d.PrivateKey))
		case wgh.DeviceAPublicKey:
			ad.Do(parseKey(&d.PublicKey))
		case wgh.DeviceAListenPort:
			d.ListenPort = int(ad.Uint16())
		case wgh.DeviceAFwmark:
			d.FirewallMark = int(ad.Uint32())
		case wgh.DeviceAPeers:
			ad.Do(func(b []byte) error {
				peers, err := parsePeers(b)
				if err != nil {
					return err
				}

				d.Peers = peers
				return nil
			})
		}
	}

	if err := ad.Err(); err != nil {
		return nil, err
	}

	return &d, nil
}

// parsePeers parses a slice of Peers from a netlink attribute payload.
func parsePeers(b []byte) ([]wgtypes.Peer, error) {
	attrs, err := netlink.UnmarshalAttributes(b)
	if err != nil {
		return nil, err
	}

	// This is a netlink "array", so each attribute's data contains more
	// nested attributes for a new Peer.
	ps := make([]wgtypes.Peer, 0, len(attrs))
	for _, a := range attrs {
		ad, err := netlink.NewAttributeDecoder(a.Data)
		if err != nil {
			return nil, err
		}

		var p wgtypes.Peer
		for ad.Next() {
			switch ad.Type() {
			case wgh.PeerAPublicKey:
				ad.Do(parseKey(&p.PublicKey))
			case wgh.PeerAPresharedKey:
				ad.Do(parseKey(&p.PresharedKey))
			case wgh.PeerAEndpoint:
				p.Endpoint = &net.UDPAddr{}
				ad.Do(parseSockaddr(p.Endpoint))
			case wgh.PeerAPersistentKeepaliveInterval:
				// TODO(mdlayher): is this actually in seconds?
				p.PersistentKeepaliveInterval = time.Duration(ad.Uint16()) * time.Second
			case wgh.PeerALastHandshakeTime:
				ad.Do(parseTimespec(&p.LastHandshakeTime))
			case wgh.PeerARxBytes:
				p.ReceiveBytes = int(ad.Uint64())
			case wgh.PeerATxBytes:
				p.TransmitBytes = int(ad.Uint64())
			case wgh.PeerAAllowedips:
				ad.Do(func(b []byte) error {
					ipns, err := parseAllowedIPs(b)
					if err != nil {
						return err
					}

					p.AllowedIPs = ipns
					return nil
				})
			}
		}

		if err := ad.Err(); err != nil {
			return nil, err
		}

		ps = append(ps, p)
	}

	return ps, nil
}

// parseAllowedIPs parses a slice of net.IPNet from a netlink attribute payload.
func parseAllowedIPs(b []byte) ([]net.IPNet, error) {
	attrs, err := netlink.UnmarshalAttributes(b)
	if err != nil {
		return nil, err
	}

	// This is a netlink "array", so each attribute's data contains more
	// nested attributes for a new net.IPNet.
	ipns := make([]net.IPNet, 0, len(attrs))
	for _, a := range attrs {
		ad, err := netlink.NewAttributeDecoder(a.Data)
		if err != nil {
			return nil, err
		}

		var (
			ipn    net.IPNet
			mask   int
			family int
		)

		for ad.Next() {
			switch ad.Type() {
			case wgh.AllowedipAIpaddr:
				ad.Do(parseAddr(&ipn.IP))
			case wgh.AllowedipACidrMask:
				mask = int(ad.Uint8())
			case wgh.AllowedipAFamily:
				family = int(ad.Uint16())
			}
		}

		if err := ad.Err(); err != nil {
			return nil, err
		}

		// The address family determines the correct number of bits in the mask.
		switch family {
		case unix.AF_INET:
			ipn.Mask = net.CIDRMask(mask, 32)
		case unix.AF_INET6:
			ipn.Mask = net.CIDRMask(mask, 128)
		}

		ipns = append(ipns, ipn)
	}

	return ipns, nil
}

// parseKey parses a wgtypes.Key from a byte slice.
func parseKey(key *wgtypes.Key) func(b []byte) error {
	return func(b []byte) error {
		k, err := wgtypes.NewKey(b)
		if err != nil {
			return err
		}

		*key = k
		return nil
	}
}

// parseAddr parses a net.IP from raw in_addr or in6_addr struct bytes.
func parseAddr(ip *net.IP) func(b []byte) error {
	return func(b []byte) error {
		switch len(b) {
		case net.IPv4len, net.IPv6len:
			// Okay to convert directly to net.IP; memory layout is identical.
			*ip = make(net.IP, len(b))
			copy(*ip, b)
			return nil
		default:
			return fmt.Errorf("wireguardnl: unexpected IP address size: %d", len(b))
		}
	}
}

// parseSockaddr parses a *net.UDPAddr from raw sockaddr_in or sockaddr_in6 bytes.
func parseSockaddr(endpoint *net.UDPAddr) func(b []byte) error {
	return func(b []byte) error {
		switch len(b) {
		case unix.SizeofSockaddrInet4:
			// IPv4 address parsing.
			sa := *(*unix.RawSockaddrInet4)(unsafe.Pointer(&b[0]))

			*endpoint = net.UDPAddr{
				IP:   net.IP(sa.Addr[:]).To4(),
				Port: int(sa.Port),
			}

			return nil
		case unix.SizeofSockaddrInet6:
			// IPv6 address parsing.
			sa := *(*unix.RawSockaddrInet6)(unsafe.Pointer(&b[0]))

			*endpoint = net.UDPAddr{
				IP:   net.IP(sa.Addr[:]),
				Port: int(sa.Port),
			}

			return nil
		default:
			return fmt.Errorf("wireguardnl: unexpected sockaddr size: %d", len(b))
		}
	}
}

const sizeofTimespec = int(unsafe.Sizeof(unix.Timespec{}))

// parseTimespec parses a time.Time from raw timespec bytes.
func parseTimespec(t *time.Time) func(b []byte) error {
	return func(b []byte) error {
		if len(b) != sizeofTimespec {
			return fmt.Errorf("wireguardnl: unexpected timespec size: %d", len(b))
		}

		ts := *(*unix.Timespec)(unsafe.Pointer(&b[0]))
		*t = time.Unix(ts.Sec, ts.Nsec)
		return nil
	}
}

// mergeDevices merges Peer information from d into target.  mergeDevices is
// used to deal with multiple incoming netlink messages for the same device.
func mergeDevices(target, d *wgtypes.Device) error {
	// Peers we are aware already exist in target.
	known := make(map[wgtypes.Key]struct{})
	for _, p := range target.Peers {
		known[p.PublicKey] = struct{}{}
	}

	// Peers which will be added to target if new peers are discovered.
	var peers []wgtypes.Peer

	for j := range target.Peers {
		// Allowed IPs that will be added to target for matching peers.
		var ipns []net.IPNet

		for k := range d.Peers {
			// Does this peer match the current peer?  If so, append its allowed
			// IP networks.
			if target.Peers[j].PublicKey == d.Peers[k].PublicKey {
				ipns = append(ipns, d.Peers[k].AllowedIPs...)
				continue
			}

			// Are we already aware of this peer's existence?  If so, nothing to
			// do here.
			if _, ok := known[d.Peers[k].PublicKey]; ok {
				continue
			}

			// Found a new peer, append it to the output list and mark it as
			// known for future loops.
			peers = append(peers, d.Peers[k])
			known[d.Peers[k].PublicKey] = struct{}{}
		}

		// Add any newly-encountered IPs for this peer.
		target.Peers[j].AllowedIPs = append(target.Peers[j].AllowedIPs, ipns...)
	}

	// Add any newly-encountered peers for this device.
	target.Peers = append(target.Peers, peers...)

	return nil
}
