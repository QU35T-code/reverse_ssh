package handlers

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"github.com/NHAS/reverse_ssh/pkg/logger"
	"github.com/NHAS/reverse_ssh/sources"
	"github.com/go-ping/ping"
	"github.com/nicocha30/gvisor-ligolo/pkg/buffer"
	"github.com/nicocha30/gvisor-ligolo/pkg/tcpip"
	"github.com/nicocha30/gvisor-ligolo/pkg/tcpip/adapters/gonet"
	"github.com/nicocha30/gvisor-ligolo/pkg/tcpip/header"
	"github.com/nicocha30/gvisor-ligolo/pkg/tcpip/network/ipv4"
	"github.com/nicocha30/gvisor-ligolo/pkg/tcpip/network/ipv6"
	"github.com/nicocha30/gvisor-ligolo/pkg/tcpip/stack"
	"github.com/nicocha30/gvisor-ligolo/pkg/tcpip/transport/icmp"
	"github.com/nicocha30/gvisor-ligolo/pkg/tcpip/transport/raw"
	"github.com/nicocha30/gvisor-ligolo/pkg/tcpip/transport/tcp"
	"github.com/nicocha30/gvisor-ligolo/pkg/tcpip/transport/udp"
	"github.com/nicocha30/gvisor-ligolo/pkg/waiter"
	"golang.org/x/crypto/ssh"
)

func Tun(_ *sources.User, newChannel ssh.NewChannel, l logger.Logger) {

	defer func() {
		if r := recover(); r != nil {
			l.Error("Recovered panic from tun driver %v", r)
		}
	}()

	var tunInfo struct {
		Mode uint32
		No   uint32
	}

	err := ssh.Unmarshal(newChannel.ExtraData(), &tunInfo)
	if err != nil {
		newChannel.Reject(ssh.ConnectionFailed, "connection closed")
		l.Warning("Unable to accept new channel %s", err)
		return
	}

	if tunInfo.Mode != 1 {
		newChannel.Reject(ssh.ConnectionFailed, "connection closed")
		return
	}

	tunnel, req, err := newChannel.Accept()
	if err != nil {
		newChannel.Reject(ssh.ConnectionFailed, "connection closed")
		l.Warning("Unable to accept new channel %s", err)
		return
	}
	defer tunnel.Close()

	// Create a new gvisor userland network stack.
	ns := stack.New(stack.Options{
		NetworkProtocols: []stack.NetworkProtocolFactory{
			ipv4.NewProtocol,
			ipv6.NewProtocol,
		},
		TransportProtocols: []stack.TransportProtocolFactory{
			tcp.NewProtocol,
			udp.NewProtocol,
			icmp.NewProtocol4,
			icmp.NewProtocol6,
		},
		HandleLocal: false,
	})
	defer ns.Close()

	linkEP := NewSSHEndpoint(tunnel)

	const NICID = 1
	// Create a new NIC
	if err := ns.CreateNIC(NICID, linkEP); err != nil {
		l.Error("CreateNIC: %v", err)
		return
	}

	err = icmpResponder(ns)
	if err != nil {
		l.Error("Unable to create icmp responder: %v", err)
		return
	}

	// Forward TCP connections
	tcpHandler := tcp.NewForwarder(ns, 30000, 4000, forwardTCP)

	// Forward UDP connections
	udpHandler := udp.NewForwarder(ns, forwardUDP(ns))

	// Register forwarders
	ns.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpHandler.HandlePacket)
	ns.SetTransportProtocolHandler(udp.ProtocolNumber, udpHandler.HandlePacket)

	// Allow all routes by default
	ns.SetRouteTable([]tcpip.Route{
		{
			Destination: header.IPv4EmptySubnet,
			NIC:         NICID,
		},
		{
			Destination: header.IPv6EmptySubnet,
			NIC:         NICID,
		},
	})

	// Disable forwarding
	ns.SetForwardingDefaultAndAllNICs(ipv4.ProtocolNumber, false)
	ns.SetForwardingDefaultAndAllNICs(ipv6.ProtocolNumber, false)

	// Enable TCP SACK
	nsacks := tcpip.TCPSACKEnabled(true)
	ns.SetTransportProtocolOption(tcp.ProtocolNumber, &nsacks)

	// Disable SYN-Cookies, as this can mess with nmap scans
	synCookies := tcpip.TCPAlwaysUseSynCookies(false)
	ns.SetTransportProtocolOption(tcp.ProtocolNumber, &synCookies)

	// Allow packets from all sources/destinations
	ns.SetPromiscuousMode(NICID, true)
	ns.SetSpoofing(NICID, true)

	ssh.DiscardRequests(req)

	l.Info("Tunnel ended")

}

func forwardUDP(stack *stack.Stack) func(request *udp.ForwarderRequest) {
	return func(request *udp.ForwarderRequest) {
		go func() {
			id := request.ID()

			var wq waiter.Queue

			ep, iperr := request.CreateEndpoint(&wq)
			if iperr != nil {
				return
			}

			log.Printf("tun [+] %s -> %s:%d/udp\n", id.RemoteAddress, id.LocalAddress, id.LocalPort)

			fwdDst := net.UDPAddr{
				IP:   net.ParseIP(id.LocalAddress.String()),
				Port: int(id.LocalPort),
			}

			remote, err := net.DialUDP("udp", nil, &fwdDst)
			if err != nil {
				return
			}

			local := gonet.NewUDPConn(stack, &wq, ep)

			err = Proxy(local, remote)
			if err != nil {
				log.Printf("proxy connection closed with error: %v", err)
			}
		}()
	}
}

func forwardTCP(request *tcp.ForwarderRequest) {
	go func() {
		id := request.ID()

		var wq waiter.Queue
		ep, errTcp := request.CreateEndpoint(&wq)
		if errTcp != nil {
			fmt.Printf("r.CreateEndpoint() = %v\n", errTcp)
			request.Complete(true)
			return
		}

		fwdDst := net.TCPAddr{
			IP:   net.ParseIP(id.LocalAddress.String()),
			Port: int(id.LocalPort),
		}

		log.Printf("[+] %s -> %s:%d/tcp\n", id.RemoteAddress, id.LocalAddress, id.LocalPort)

		local := gonet.NewTCPConn(&wq, ep)

		remote, err := net.Dial("tcp", fwdDst.String())
		if err != nil {
			fmt.Println(err)
			request.Complete(true)
			return
		}

		err = Proxy(local, remote)
		if err != nil {
			log.Printf("proxy connection closed with error: %v", err)
		}
	}()
}

func Proxy(c1, c2 net.Conn) error {
	connClosed := make(chan error, 2)

	defer c1.Close()
	defer c2.Close()

	go func() {
		_, err := io.Copy(c1, c2)
		connClosed <- err
	}()

	go func() {
		_, err := io.Copy(c2, c1)
		connClosed <- err
	}()

	err := <-connClosed
	if err != nil {
		return err
	}

	return nil
}

type SSHEndpoint struct {
	dispatcher stack.NetworkDispatcher
	tunnel     ssh.Channel
}

func NewSSHEndpoint(dev ssh.Channel) *SSHEndpoint {
	return &SSHEndpoint{
		tunnel: dev,
	}
}

// MTU implements stack.LinkEndpoint.
func (m *SSHEndpoint) MTU() uint32 {
	return 1500
}

// Capabilities implements stack.LinkEndpoint.
func (m *SSHEndpoint) Capabilities() stack.LinkEndpointCapabilities {
	return stack.CapabilityNone
}

// MaxHeaderLength implements stack.LinkEndpoint.
func (m *SSHEndpoint) MaxHeaderLength() uint16 {
	return 0
}

// LinkAddress implements stack.LinkEndpoint.
func (m *SSHEndpoint) LinkAddress() tcpip.LinkAddress {
	return ""
}

// Attach implements stack.LinkEndpoint.
func (m *SSHEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	m.dispatcher = dispatcher
	go m.dispatchLoop()
}

func (m *SSHEndpoint) dispatchLoop() {
	for {
		packet := make([]byte, 1504)

		n, err := m.tunnel.Read(packet)
		if err != nil {
			break
		}

		if len(packet) < 4 {
			continue
		}

		//Remove the SSH added family address uint32 (for layer 3 tun)
		packet = packet[4:]

		if !m.IsAttached() {
			continue
		}

		pkb := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.NewWithData(packet[:n-4]),
		})

		switch header.IPVersion(packet) {
		case header.IPv4Version:
			m.dispatcher.DeliverNetworkPacket(header.IPv4ProtocolNumber, pkb)
		case header.IPv6Version:
			m.dispatcher.DeliverNetworkPacket(header.IPv6ProtocolNumber, pkb)
		}
	}
}

// IsAttached implements stack.LinkEndpoint.
func (m *SSHEndpoint) IsAttached() bool {
	return m.dispatcher != nil
}

// WritePackets writes outbound packets
func (m *SSHEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	n := 0
	for _, pkt := range pkts.AsSlice() {
		if err := m.WritePacket(pkt); err != nil {
			break
		}
		n++
	}
	return n, nil
}

var lock sync.Mutex

// WritePacket writes outbound packets
func (m *SSHEndpoint) WritePacket(pkt *stack.PacketBuffer) tcpip.Error {

	pktBuf := pkt.Buffer()

	//I have quite literally no idea why a lock here fixes ssh issues
	lock.Lock()
	defer lock.Unlock()

	// 3.2 Frame Format
	// https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git/tree/Documentation/networking/tuntap.rst?id=HEAD
	packet := make([]byte, 4+pktBuf.Size())
	binary.BigEndian.PutUint16(packet, 1)
	binary.BigEndian.PutUint16(packet[2:], uint16(pkt.NetworkProtocolNumber))

	copy(packet[4:], pktBuf.Flatten())

	if _, err := m.tunnel.Write(packet); err != nil {
		return &tcpip.ErrInvalidEndpointState{}
	}
	return nil
}

// Wait implements stack.LinkEndpoint.Wait.
func (m *SSHEndpoint) Wait() {}

// ARPHardwareType implements stack.LinkEndpoint.ARPHardwareType.
func (*SSHEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone
}

// AddHeader implements stack.LinkEndpoint.AddHeader.
func (*SSHEndpoint) AddHeader(pkt *stack.PacketBuffer) {
}

// WriteRawPacket implements stack.LinkEndpoint.
func (*SSHEndpoint) WriteRawPacket(*stack.PacketBuffer) tcpip.Error {
	return &tcpip.ErrNotSupported{}
}

func icmpResponder(s *stack.Stack) error {

	var wq waiter.Queue
	rawProto, rawerr := raw.NewEndpoint(s, ipv4.ProtocolNumber, icmp.ProtocolNumber4, &wq)
	if rawerr != nil {
		return errors.New("could not create raw endpoint")
	}
	if err := rawProto.Bind(tcpip.FullAddress{}); err != nil {
		return errors.New("could not bind raw endpoint")
	}
	go func() {
		we, ch := waiter.NewChannelEntry(waiter.ReadableEvents)
		wq.EventRegister(&we)
		for {
			var buff bytes.Buffer
			_, err := rawProto.Read(&buff, tcpip.ReadOptions{})

			if _, ok := err.(*tcpip.ErrWouldBlock); ok {
				// Wait for data to become available.
				select {
				case <-ch:
					_, err := rawProto.Read(&buff, tcpip.ReadOptions{})

					if err != nil {

						continue
					}

					iph := header.IPv4(buff.Bytes())

					hlen := int(iph.HeaderLength())
					if buff.Len() < hlen {
						return
					}

					// Reconstruct a ICMP PacketBuffer from bytes.
					view := buffer.NewWithData(buff.Bytes())
					packetbuff := stack.NewPacketBuffer(stack.PacketBufferOptions{
						Payload:            view,
						ReserveHeaderBytes: hlen,
					})

					packetbuff.NetworkProtocolNumber = ipv4.ProtocolNumber
					packetbuff.TransportProtocolNumber = icmp.ProtocolNumber4
					packetbuff.NetworkHeader().Consume(hlen)

					go func() {
						if TryResolve(iph.DestinationAddress().String()) {
							ProcessICMP(s, packetbuff)
						}
					}()

				}
			}

		}
	}()
	return nil
}

// ProcessICMP send back a ICMP echo reply from after receiving a echo request.
// This code come mostly from pkg/tcpip/network/ipv4/icmp.go
func ProcessICMP(nstack *stack.Stack, pkt *stack.PacketBuffer) {
	// (gvisor) pkg/tcpip/network/ipv4/icmp.go:174 - handleICMP

	// ICMP packets don't have their TransportHeader fields set. See
	// icmp/protocol.go:protocol.Parse for a full explanation.
	v, ok := pkt.Data().PullUp(header.ICMPv4MinimumSize)
	if !ok {
		return
	}
	h := header.ICMPv4(v)

	// Only do in-stack processing if the checksum is correct.
	if pkt.Data().AsRange().Checksum() != 0xffff {
		return
	}

	iph := header.IPv4(pkt.NetworkHeader().View())
	var newOptions header.IPv4Options

	// TODO(b/112892170): Meaningfully handle all ICMP types.
	switch h.Type() {
	case header.ICMPv4Echo:

		replyData := pkt.Data().AsRange().ToOwnedView()
		ipHdr := header.IPv4(pkt.NetworkHeader().View())

		localAddressBroadcast := pkt.NetworkPacketInfo.LocalAddressBroadcast

		// It's possible that a raw socket expects to receive this.
		pkt = nil

		// Take the base of the incoming request IP header but replace the options.
		replyHeaderLength := uint8(header.IPv4MinimumSize + len(newOptions))
		replyIPHdr := header.IPv4(append(iph[:header.IPv4MinimumSize:header.IPv4MinimumSize], newOptions...))
		replyIPHdr.SetHeaderLength(replyHeaderLength)

		// As per RFC 1122 section 3.2.1.3, when a host sends any datagram, the IP
		// source address MUST be one of its own IP addresses (but not a broadcast
		// or multicast address).
		localAddr := ipHdr.DestinationAddress()
		if localAddressBroadcast || header.IsV4MulticastAddress(localAddr) {
			localAddr = ""
		}

		r, err := nstack.FindRoute(1, localAddr, ipHdr.SourceAddress(), ipv4.ProtocolNumber, false /* multicastLoop */)
		if err != nil {
			// If we cannot find a route to the destination, silently drop the packet.
			return
		}
		defer r.Release()

		replyIPHdr.SetSourceAddress(r.LocalAddress())
		replyIPHdr.SetDestinationAddress(r.RemoteAddress())
		replyIPHdr.SetTTL(r.DefaultTTL())

		replyICMPHdr := header.ICMPv4(replyData)
		replyICMPHdr.SetType(header.ICMPv4EchoReply)
		replyICMPHdr.SetChecksum(0)
		replyICMPHdr.SetChecksum(^header.Checksum(replyData, 0))

		replyBuf := buffer.NewWithData(replyIPHdr)
		replyBuf.AppendOwned(replyData)
		replyPkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			ReserveHeaderBytes: int(r.MaxHeaderLength()),
			Payload:            replyBuf,
		})

		replyPkt.TransportProtocolNumber = header.ICMPv4ProtocolNumber

		if err := r.WriteHeaderIncludedPacket(replyPkt); err != nil {
			return
		}
	}
}

// TryResolve tries to discover if the remote host is up using ICMP
func TryResolve(address string) bool {
	methods := []func(string) (bool, error){
		RawPinger,
		CommandPinger,
	}
	for _, method := range methods {
		if result, err := method(address); err == nil {
			return result
		}
	}
	// Everything failed...
	return false
}

// RawPinger use ICMP sockets to discover if a host is up. This could require administrative permissions on some hosts
func RawPinger(target string) (bool, error) {
	pinger, err := ping.NewPinger(target)
	if err != nil {
		return false, err
	}
	pinger.Count = 1
	pinger.Timeout = 4 * time.Second // NMAP default timeout ?
	if runtime.GOOS == "windows" {
		pinger.SetPrivileged(true)
	}
	err = pinger.Run()
	if err != nil {
		return false, err
	}

	return pinger.PacketsRecv != 0, nil
}

// CommandPinger uses the sources ping command (dirty), but should not require privileges
func CommandPinger(target string) (bool, error) {
	countArg := "-c"
	waitArg := "-W"
	waitTime := "3"
	if runtime.GOOS == "windows" {
		countArg = "/n"
		waitArg = "/w"
		waitTime = "3000"
	}

	cmd := exec.Command("ping", countArg, "1", waitArg, waitTime, target)
	if err := cmd.Run(); err != nil {
		return false, err
	}
	return true, nil
}
