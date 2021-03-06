package portmapper

import (
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/docker/libnetwork/ip6tables"
	"github.com/docker/libnetwork/iptables"
	"github.com/docker/libnetwork/portallocator"
	"github.com/ishidawataru/sctp"
	"github.com/sirupsen/logrus"
)

type mapping struct {
	proto         string
	userlandProxy userlandProxy
	host          net.Addr
	container     net.Addr
	containerv6   net.Addr
}

var newProxy = newProxyCommand

var (
	// ErrUnknownBackendAddressType refers to an unknown container or unsupported address type
	ErrUnknownBackendAddressType = errors.New("unknown container address type not supported")
	// ErrPortMappedForIP refers to a port already mapped to an ip address
	ErrPortMappedForIP = errors.New("port is already mapped to ip")
	// ErrPortNotMapped refers to an unmapped port
	ErrPortNotMapped = errors.New("port is not mapped")
	// ErrSCTPAddrNoIP refers to a SCTP address without IP address.
	ErrSCTPAddrNoIP = errors.New("sctp address does not contain any IP address")
)

// PortMapper manages the network address translation
type PortMapper struct {
	chain      *iptables.ChainInfo
	ip6tChain  *ip6tables.ChainInfo
	bridgeName string

	// udp:ip:port
	currentMappings map[string]*mapping
	lock            sync.Mutex

	proxyPath string

	Allocator *portallocator.PortAllocator
}

// New returns a new instance of PortMapper
func New(proxyPath string) *PortMapper {
	return NewWithPortAllocator(portallocator.Get(), proxyPath)
}

// NewWithPortAllocator returns a new instance of PortMapper which will use the specified PortAllocator
func NewWithPortAllocator(allocator *portallocator.PortAllocator, proxyPath string) *PortMapper {
	return &PortMapper{
		currentMappings: make(map[string]*mapping),
		Allocator:       allocator,
		proxyPath:       proxyPath,
	}
}

// SetIptablesChain sets the specified chain into portmapper
func (pm *PortMapper) SetIptablesChain(c *iptables.ChainInfo, bridgeName string) {
	pm.chain = c
	pm.bridgeName = bridgeName
}

// SetIP6tablesChain sets the specified chain into portmapper
func (pm *PortMapper) SetIP6tablesChain(c *ip6tables.ChainInfo, bridgeName string) {
	pm.ip6tChain = c
	pm.bridgeName = bridgeName
}

// Map maps the specified container transport address to the host's network address and transport port
func (pm *PortMapper) Map(container net.Addr, containerv6 net.Addr, hostIP net.IP, hostPort int, useProxy bool) (host net.Addr, err error) {
	return pm.MapRange(container, containerv6, hostIP, hostPort, hostPort, useProxy)
}

// MapRange maps the specified container transport address to the host's network address and transport port range
func (pm *PortMapper) MapRange(container net.Addr, containerv6 net.Addr, hostIP net.IP, hostPortStart, hostPortEnd int, useProxy bool) (host net.Addr, err error) {
	pm.lock.Lock()
	defer pm.lock.Unlock()

	var (
		m                 *mapping
		proto             string
		allocatedHostPort int
	)

	switch container.(type) {
	case *net.TCPAddr:
		proto = "tcp"
		if allocatedHostPort, err = pm.Allocator.RequestPortInRange(hostIP, proto, hostPortStart, hostPortEnd); err != nil {
			return nil, err
		}

		m = &mapping{
			proto:       proto,
			host:        &net.TCPAddr{IP: hostIP, Port: allocatedHostPort},
			container:   container,
			containerv6: containerv6,
		}

		if useProxy {
			m.userlandProxy, err = newProxy(proto, hostIP, allocatedHostPort, container.(*net.TCPAddr).IP, container.(*net.TCPAddr).Port, pm.proxyPath)
			if err != nil {
				return nil, err
			}
		} else {
			m.userlandProxy, err = newDummyProxy(proto, hostIP, allocatedHostPort)
			if err != nil {
				return nil, err
			}
		}
	case *net.UDPAddr:
		proto = "udp"
		if allocatedHostPort, err = pm.Allocator.RequestPortInRange(hostIP, proto, hostPortStart, hostPortEnd); err != nil {
			return nil, err
		}

		m = &mapping{
			proto:       proto,
			host:        &net.UDPAddr{IP: hostIP, Port: allocatedHostPort},
			container:   container,
			containerv6: containerv6,
		}

		if useProxy {
			m.userlandProxy, err = newProxy(proto, hostIP, allocatedHostPort, container.(*net.UDPAddr).IP, container.(*net.UDPAddr).Port, pm.proxyPath)
			if err != nil {
				return nil, err
			}
		} else {
			m.userlandProxy, err = newDummyProxy(proto, hostIP, allocatedHostPort)
			if err != nil {
				return nil, err
			}
		}
	case *sctp.SCTPAddr:
		proto = "sctp"
		if allocatedHostPort, err = pm.Allocator.RequestPortInRange(hostIP, proto, hostPortStart, hostPortEnd); err != nil {
			return nil, err
		}

		m = &mapping{
			proto:       proto,
			host:        &sctp.SCTPAddr{IP: []net.IP{hostIP}, Port: allocatedHostPort},
			container:   container,
			containerv6: containerv6,
		}

		if useProxy {
			sctpAddr := container.(*sctp.SCTPAddr)
			if len(sctpAddr.IP) == 0 {
				return nil, ErrSCTPAddrNoIP
			}
			m.userlandProxy, err = newProxy(proto, hostIP, allocatedHostPort, sctpAddr.IP[0], sctpAddr.Port, pm.proxyPath)
			if err != nil {
				return nil, err
			}
		} else {
			m.userlandProxy, err = newDummyProxy(proto, hostIP, allocatedHostPort)
			if err != nil {
				return nil, err
			}
		}
	default:
		return nil, ErrUnknownBackendAddressType
	}

	// release the allocated port on any further error during return.
	defer func() {
		if err != nil {
			pm.Allocator.ReleasePort(hostIP, proto, allocatedHostPort)
		}
	}()

	key := getKey(m.host)
	if _, exists := pm.currentMappings[key]; exists {
		return nil, ErrPortMappedForIP
	}

	containerIP, containerPort := getIPAndPort(m.container)
	if containerIP.To4() != nil {
		if err := pm.forward(iptables.Append, m.proto, hostIP, allocatedHostPort, containerIP.String(), containerPort); err != nil {
			return nil, err
		}
	}
	containerIPv6, containerPort := getIPAndPort(m.containerv6)
	if containerIPv6 != nil {
		if err := pm.ip6tForward(ip6tables.Append, m.proto, hostIP, allocatedHostPort, containerIPv6.String(), containerPort); err != nil {
			return nil, err
		}
	}

	cleanup := func() error {
		// need to undo the iptables rules before we return
		m.userlandProxy.Stop()
		if containerIP.To4() != nil {
			pm.forward(iptables.Delete, m.proto, hostIP, allocatedHostPort, containerIP.String(), containerPort)
			if err := pm.Allocator.ReleasePort(hostIP, m.proto, allocatedHostPort); err != nil {
				return err
			}
		}
		if containerIPv6 != nil {
			pm.ip6tForward(ip6tables.Delete, m.proto, hostIP, allocatedHostPort, containerIPv6.String(), containerPort)
			if err := pm.Allocator.ReleasePort(hostIP, m.proto, allocatedHostPort); err != nil {
				return err
			}
		}

		return nil
	}

	if err := m.userlandProxy.Start(); err != nil {
		if err := cleanup(); err != nil {
			return nil, fmt.Errorf("Error during port allocation cleanup: %v", err)
		}
		return nil, err
	}

	pm.currentMappings[key] = m
	return m.host, nil
}

// Unmap removes stored mapping for the specified host transport address
func (pm *PortMapper) Unmap(host net.Addr) error {
	pm.lock.Lock()
	defer pm.lock.Unlock()

	key := getKey(host)
	data, exists := pm.currentMappings[key]
	if !exists {
		return ErrPortNotMapped
	}

	if data.userlandProxy != nil {
		data.userlandProxy.Stop()
	}

	delete(pm.currentMappings, key)

	containerIP, containerPort := getIPAndPort(data.container)
	hostIP, hostPort := getIPAndPort(data.host)
	if err := pm.forward(iptables.Delete, data.proto, hostIP, hostPort, containerIP.String(), containerPort); err != nil {
		logrus.Errorf("Error on iptables delete: %s", err)
	}
	containerIPv6, containerPort := getIPAndPort(data.containerv6)
	if containerIPv6 != nil {
		if err := pm.ip6tForward(ip6tables.Delete, data.proto, hostIP, hostPort, containerIPv6.String(), containerPort); err != nil {
			logrus.Errorf("Error on ip6tables delete: %s", err)
		}
	}

	switch a := host.(type) {
	case *net.TCPAddr:
		return pm.Allocator.ReleasePort(a.IP, "tcp", a.Port)
	case *net.UDPAddr:
		return pm.Allocator.ReleasePort(a.IP, "udp", a.Port)
	case *sctp.SCTPAddr:
		if len(a.IP) == 0 {
			return ErrSCTPAddrNoIP
		}
		return pm.Allocator.ReleasePort(a.IP[0], "sctp", a.Port)
	}
	return ErrUnknownBackendAddressType
}

//ReMapAll will re-apply all port mappings
func (pm *PortMapper) ReMapAll() {
	pm.lock.Lock()
	defer pm.lock.Unlock()
	logrus.Debugln("Re-applying all port mappings.")
	for _, data := range pm.currentMappings {
		containerIP, containerPort := getIPAndPort(data.container)
		hostIP, hostPort := getIPAndPort(data.host)
		if err := pm.forward(iptables.Append, data.proto, hostIP, hostPort, containerIP.String(), containerPort); err != nil {
			logrus.Errorf("Error on iptables add: %s", err)
		}
		if data.containerv6 != nil {
			containerIPv6, containerPort := getIPAndPort(data.containerv6)
			if err := pm.ip6tForward(ip6tables.Append, data.proto, hostIP, hostPort, containerIPv6.String(), containerPort); err != nil {
				logrus.Errorf("Error on ip6tables add: %s", err)
			}
		}
	}
}

func getKey(a net.Addr) string {
	switch t := a.(type) {
	case *net.TCPAddr:
		return fmt.Sprintf("%s:%d/%s", t.IP.String(), t.Port, "tcp")
	case *net.UDPAddr:
		return fmt.Sprintf("%s:%d/%s", t.IP.String(), t.Port, "udp")
	case *sctp.SCTPAddr:
		if len(t.IP) == 0 {
			logrus.Error(ErrSCTPAddrNoIP)
			return ""
		}
		return fmt.Sprintf("%s:%d/%s", t.IP[0].String(), t.Port, "sctp")
	}
	return ""
}

func getIPAndPort(a net.Addr) (net.IP, int) {
	switch t := a.(type) {
	case *net.TCPAddr:
		return t.IP, t.Port
	case *net.UDPAddr:
		return t.IP, t.Port
	case *sctp.SCTPAddr:
		if len(t.IP) == 0 {
			logrus.Error(ErrSCTPAddrNoIP)
			return nil, 0
		}
		return t.IP[0], t.Port
	}
	return nil, 0
}

func (pm *PortMapper) forward(action iptables.Action, proto string, sourceIP net.IP, sourcePort int, containerIP string, containerPort int) error {
	if pm.chain == nil {
		return nil
	}
	return pm.chain.Forward(action, sourceIP, sourcePort, proto, containerIP, containerPort, pm.bridgeName)
}

func (pm *PortMapper) ip6tForward(action ip6tables.Action, proto string, sourceIP net.IP, sourcePort int, containerIPv6 string, containerPort int) error {
	if pm.ip6tChain == nil {
		return nil
	}
	return pm.ip6tChain.Forward(action, sourceIP, sourcePort, proto, containerIPv6, containerPort, pm.bridgeName)
}
