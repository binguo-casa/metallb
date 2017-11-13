package main

import (
	"errors"
	"fmt"
	"net"

	"go.universe.tf/metallb/internal"
	"go.universe.tf/metallb/internal/config"
	"k8s.io/api/core/v1"
)

func (c *controller) convergeService(key string, svc *v1.Service) {
	// The assigned IP annotation is the end state of convergence. If
	// there's none or a malformed one, nuke all controlled state so
	// that we start converging from a clean slate.
	lbIP := net.ParseIP(svc.Annotations[internal.AnnotationAssignedIP]).To4()
	if lbIP == nil {
		c.clearServiceState(key, svc)
	}

	// It's possible the config mutated and the IP we have no longer
	// makes sense. If so, clear it out and give the rest of the logic
	// a chance to allocate again.
	if lbIP != nil && !c.ipIsValid(lbIP) {
		c.clearServiceState(key, svc)
	}

	// User set or changed the desired LB IP, nuke the
	// state. allocateIP will pay attention to LoadBalancerIP and try
	// to meet the user's demands.
	if svc.Spec.LoadBalancerIP != "" && svc.Spec.LoadBalancerIP != svc.Annotations[internal.AnnotationAssignedIP] {
		c.clearServiceState(key, svc)
	}

	// If lbIP is still nil at this point, try to allocate.
	if lbIP == nil {
		ip, err := c.allocateIP(key, svc)
		if err != nil {
			c.events.Eventf(svc, v1.EventTypeWarning, "AllocationFailed", "Failed to allocate IP for %q: %s", key, err)
			// TODO: should retry on pool exhaustion allocation
			// failures, once we keep track of when pools become
			// non-full.
			return
		}
		lbIP = ip
	}

	if lbIP == nil {
		c.events.Eventf(svc, v1.EventTypeWarning, "InternalError", "didn't allocate an IP but also did not fail")
		return
	}

	// At this point, we have an IP selected somehow, all that remains
	// is to program the data plane.
	svc.Spec.ExternalTrafficPolicy = v1.ServiceExternalTrafficPolicyTypeLocal
	svc.Status.LoadBalancer.Ingress = []v1.LoadBalancerIngress{{IP: lbIP.String()}}
}

// clearServiceState clears all fields that are actively managed by
// this controller.
func (c *controller) clearServiceState(key string, svc *v1.Service) {
	c.Lock()
	defer c.Unlock()
	delete(c.ipToSvc, c.svcToIP[key])
	delete(c.svcToIP, key)
	delete(svc.Annotations, internal.AnnotationAssignedIP)
	svc.Status.LoadBalancer = v1.LoadBalancerStatus{}
}

// ipIsValid checks that ip is part of a configured pool.
func (c *controller) ipIsValid(ip net.IP) bool {
	for _, p := range c.config.Pools {
		for _, c := range p.CIDR {
			if c.Contains(ip) {
				return true
			}
		}
	}
	return false
}

func (c *controller) allocateIP(key string, svc *v1.Service) (net.IP, error) {
	c.Lock()
	defer c.Unlock()

	// If the user asked for a specific IP, try that.
	if svc.Spec.LoadBalancerIP != "" {
		ip := net.ParseIP(svc.Spec.LoadBalancerIP).To4()
		if ip == nil {
			return nil, fmt.Errorf("invalid spec.loadBalancerIP %q", svc.Spec.LoadBalancerIP)
		}
		if err := c.assignIP(key, svc, ip); err != nil {
			return nil, err
		}
		return ip, nil
	}

	// Otherwise, did the user ask for a specific pool?
	desiredPool := svc.Annotations[internal.AnnotationAddressPool]
	if desiredPool != "" {
		if p, ok := c.config.Pools[desiredPool]; ok {
			return c.allocateIPFromPool(key, svc, p)
		}
		return nil, fmt.Errorf("pool %q does not exist", desiredPool)
	}

	// Okay, in that case just bruteforce across all pools.
	for _, p := range c.config.Pools {
		ip, err := c.allocateIPFromPool(key, svc, p)
		if err != nil {
			return nil, err
		}
		if ip != nil {
			return ip, nil
		}
	}
	return nil, errors.New("no addresses available in any pool")
}

func (c *controller) allocateIPFromPool(key string, svc *v1.Service, pool *config.Pool) (net.IP, error) {
	for _, cidr := range pool.CIDR {
		for ip := cidr.IP; cidr.Contains(ip); ip = nextIP(ip) {
			if _, ok := c.ipToSvc[ip.String()]; !ok {
				// Minor inefficiency here, assignIP will
				// retraverse the pools to check that ip is
				// contained within a pool. TODO: refactor to
				// avoid.
				err := c.assignIP(key, svc, ip)
				if err != nil {
					return nil, err
				}
				return ip, nil
			}
		}
	}
	return nil, nil
}

func (c *controller) assignIP(key string, svc *v1.Service, ip net.IP) error {
	if s, ok := c.ipToSvc[ip.String()]; ok && s != key {
		return fmt.Errorf("address already belongs to other service %q", s)
	}

	if !c.ipIsValid(ip) {
		return errors.New("address is not part of any known pool")
	}

	c.ipToSvc[ip.String()] = key
	c.svcToIP[key] = ip.String()
	svc.Annotations[internal.AnnotationAssignedIP] = ip.String()
	c.events.Eventf(svc, v1.EventTypeNormal, "IPAllocated", "Assigned IP %q", ip)
	return nil
}

func nextIP(prev net.IP) net.IP {
	var ip net.IP
	ip = append(ip, prev...)
	if ip.To4() != nil {
		ip = ip.To4()
	}
	for o := 0; o < len(ip); o++ {
		if ip[len(ip)-o-1] != 255 {
			ip[len(ip)-o-1]++
			return ip
		}
		ip[len(ip)-o-1] = 0
	}
	return ip
}