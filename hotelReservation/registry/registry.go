package registry

import (
	"fmt"
	"net"
	"os"

	consul "github.com/hashicorp/consul/api"
	"github.com/rs/zerolog/log"
)

// NewClient returns a new Client with connection to consul
func NewClient(addr string) (*Client, error) {
	cfg := consul.DefaultConfig()
	cfg.Address = addr

	c, err := consul.NewClient(cfg)
	if err != nil {
		return nil, err
	}

	return &Client{c}, nil
}

// Client provides an interface for communicating with registry
type Client struct {
	*consul.Client
}

// Look for the network device being dedicated for gRPC traffic.
// The network CDIR should be specified in os environment
// "DSB_HOTELRESERV_GRPC_NETWORK".
// If not found, return the first non loopback IP address.
func getLocalIP() (string, error) {
	var ipGrpc string
	var ips []net.IP

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() != nil {
				ips = append(ips, ipnet.IP)
			}
		}
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("registry: can not find local ip")
	} else if len(ips) > 1 {
		// by default, return the first network IP address found.
		ipGrpc = ips[0].String()

		grpcNet := os.Getenv("DSB_GRPC_NETWORK")
		_, ipNetGrpc, err := net.ParseCIDR(grpcNet)
		if err != nil {
			log.Error().Msgf("An invalid network CIDR is set in environment DSB_HOTELRESERV_GRPC_NETWORK: %v", grpcNet)
		} else {
			for _, ip := range ips {
				if ipNetGrpc.Contains(ip) {
					ipGrpc = ip.String()
					log.Info().Msgf("gRPC traffic is routed to the dedicated network %s", ipGrpc)
					break
				}
			}
		}
	} else {
		// only one network device existed
		ipGrpc = ips[0].String()
	}

	return ipGrpc, nil
}

// Register a service with registry.
//
// Best-effort: a failure is logged and swallowed rather than returned. Nothing
// in this benchmark reads the registry back - dialer.Dial hands the service name
// straight to grpc.Dial, so peers are resolved by Kubernetes DNS, and
// WithBalancer ignores the *consul.Client it is given. The helm chart leaves the
// consul dependency switched off (Chart.yaml: condition: consul.enabled), so
// returning the error here killed every service at startup with
//
//   failed register: Put "http://consul-hotelres:8500/...": no such host
//
// which is what the image this cluster previously ran, "remove-consul", exists
// to avoid. If consul is ever enabled, registration simply starts succeeding.
func (c *Client) Register(name string, id string, ip string, port int) error {
	if ip == "" {
		var err error
		ip, err = getLocalIP()
		if err != nil {
			log.Warn().Msgf("registry: cannot determine local ip, skipping registration: %v", err)
			return nil
		}
	}
	reg := &consul.AgentServiceRegistration{
		ID:      id,
		Name:    name,
		Port:    port,
		Address: ip,
	}
	log.Info().Msgf("Trying to register service [ name: %s, id: %s, address: %s:%d ]", name, id, ip, port)

	if err := c.Agent().ServiceRegister(reg); err != nil {
		log.Warn().Msgf("registry: service registration failed, continuing without it: %v", err)
	}

	return nil
}

// Deregister removes the service address from registry. Best-effort, for the
// same reason as Register.
func (c *Client) Deregister(id string) error {
	if err := c.Agent().ServiceDeregister(id); err != nil {
		log.Warn().Msgf("registry: deregistration failed: %v", err)
	}

	return nil
}
