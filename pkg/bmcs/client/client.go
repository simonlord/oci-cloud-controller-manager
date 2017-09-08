// Copyright 2017 The Oracle Kubernetes Cloud Controller Manager Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/golang/glog"
	api "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"

	baremetal "github.com/oracle/bmcs-go-sdk"
)

const (
	// WorkRequestMaxRetries is the number of times a work request should be polled
	WorkRequestMaxRetries = 120
	// WorkRequestWaitInterval is the interval between polling a work request
	WorkRequestWaitInterval = 1 * time.Second
	// DefaultLoadBalancerPolicy is the default load balancing selection when creating a backend set
	DefaultLoadBalancerPolicy = "ROUND_ROBIN"
)

// ociHostnameTemplate is a template for a BMCS instance FQDN.
// hostnameLabel.dnsLabel.vcnDomainName
// e.g. worker-1.ad1.k8sdns.oraclevcn.com
const ociHostnameTemplate = "%s.%s.%s"

// Interface abstracts the BMCS SDK and application specific convenience
// methods for interacting with the BMCS API.
type Interface interface {
	BaremetalInterface

	// Compartment sets the compartment associated with the client instance.
	Compartment(id string) Interface

	// GetInstanceByNodeName retrieves the baremetal.Instance corresponding or
	// a SearchError if no instance matching the node name is found.
	GetInstanceByNodeName(name string) (*baremetal.Instance, error)
	// GetNodeAddressesForInstance gets the node addresses of an instance based
	// on the given instance OCID.
	GetNodeAddressesForInstance(id string) ([]api.NodeAddress, error)
	// GetAttachedVnicsForInstance returns a slice of AVAILABLE Vnics for a
	// given instance ocid.
	GetAttachedVnicsForInstance(id string) ([]*baremetal.Vnic, error)

	// CreateAndAwaitLoadBalancer creates a load balancer and blocks until data
	// is available or timeout is reached.
	CreateAndAwaitLoadBalancer(name, shape string, subnets []string) (*baremetal.LoadBalancer, error)
	// GetLoadBalancerByName gets a load balancer by its DisplayName.
	GetLoadBalancerByName(name string) (*baremetal.LoadBalancer, error)
	// CreateAndAwaitBackendSet creates the given BackendSet for the given
	// LoadBalancer.
	CreateAndAwaitBackendSet(lb *baremetal.LoadBalancer, bs baremetal.BackendSet) (*baremetal.BackendSet, error)
	// CreateAndAwaitListener creates the given Listener for the given
	// LoadBalancer.
	CreateAndAwaitListener(lb *baremetal.LoadBalancer, listener baremetal.Listener) error
	AwaitWorkRequest(id string) (*baremetal.WorkRequest, error)

	// GetSubnets returns the Subnets corresponding to the given OCIDs.
	GetSubnets(ocids []string) ([]*baremetal.Subnet, error)
	// GetSubnetsForInternalIPs returns the deduplicated subnets in which the
	// given internal IP addresses reside.
	GetSubnetsForInternalIPs(ips []string) ([]*baremetal.Subnet, error)
	// GetDefaultSecurityList gets the default SecurityList for the given Subnet
	// by assuming that the default SecurityList is always the oldest (as it is
	// created automatically when the Subnet is created and cannot be deleted).
	GetDefaultSecurityList(subnet *baremetal.Subnet) (*baremetal.SecurityList, error)
}

// BaremetalInterface defines the subset of the baremetal API exposed by the
// client. It is composed into Interface.
type BaremetalInterface interface {
	Validate() error
	GetInstance(id string) (*baremetal.Instance, error)
	CreateBackend(loadBalancerID string, backendSetName string, ipAddr string, port int, opts *baremetal.CreateLoadBalancerBackendOptions) (string, error)
	UpdateSecurityList(id string, opts *baremetal.UpdateSecurityListOptions) (*baremetal.SecurityList, error)
	DeleteBackendSet(loadBalancerID string, backendSetName string, opts *baremetal.ClientRequestOptions) (string, error)
	DeleteBackend(loadBalancerID string, backendSetName string, backendName string, opts *baremetal.ClientRequestOptions) (string, error)
	DeleteListener(loadBalancerID string, listenerName string, opts *baremetal.ClientRequestOptions) (string, error)
	DeleteLoadBalancer(id string, opts *baremetal.ClientRequestOptions) (string, error)
}

// New creates a new BMCS API client.
func New(cfg *Config) (Interface, error) {
	privateKeyFile := baremetal.PrivateKeyFilePath(cfg.Global.PrivateKeyFile)
	region := baremetal.Region(cfg.Global.Region)
	bmcsClient, err := baremetal.NewClient(
		cfg.Global.UserOCID,
		cfg.Global.TenancyOCID,
		cfg.Global.Fingerprint,
		privateKeyFile,
		region)
	if err != nil {
		return nil, err
	}

	return &client{Client: bmcsClient}, nil
}

// client is a wrapped baremetal.Client with additional methods/props for
// convenience.
type client struct {
	*baremetal.Client

	// OCID of the compartment of the instance the CCM is executing on.
	compartmentOCID string
}

// Just check we can talk to baremetal before doing anything else (failfast)
// Maybe do some more things like check the compartment we are give is valid....
func (c *client) Validate() error {
	_, err := c.Client.ListAvailabilityDomains(c.compartmentOCID)
	return err
}

// Compartment sets the compartment associated with the client instance.
func (c *client) Compartment(id string) Interface {
	c.compartmentOCID = id
	return c
}

// GetInstanceByNodeName gets the OCID of instance with a display name equal to
// the given node name.
// FIXME (apryde): Would be better to use vnic hostnameLabel but it would
// require a ton of queries.
func (c *client) GetInstanceByNodeName(nodeName string) (*baremetal.Instance, error) {
	glog.V(4).Infof("getInstanceByNodeName(%q) called", nodeName)
	if nodeName == "" {
		return nil, fmt.Errorf("blank nodeName passed to getInstanceByNodeName()")
	}

	opts := &baremetal.ListInstancesOptions{
		DisplayNameListOptions: baremetal.DisplayNameListOptions{
			DisplayName: nodeName,
		},
	}
	var running []baremetal.Instance
	for {
		r, err := c.ListInstances(c.compartmentOCID, opts)
		if err != nil {
			return nil, err
		}

		for _, i := range r.Instances {
			if i.State == baremetal.ResourceRunning {
				running = append(running, i)
			}
		}

		if hasNexPage := SetNextPageOption(r.NextPage, &opts.ListOptions.PageListOptions); !hasNexPage {
			break
		}
	}

	count := len(running)
	switch {
	case count == 0:
		// If we can't find an instance by display name fall back to the more
		// expensive search method.
		return c.findInstanceByNodeNameIsVnic(nodeName)
	case count > 1:
		return nil, fmt.Errorf("expected one instance with display name '%s' but got %d", nodeName, count)
	}

	glog.V(4).Infof("getInstanceByNodeName(%q): Got instance %s", nodeName, running[0].ID)
	return &running[0], nil
}

// findInstanceByNodeNameIsVnic tries to find the BMC Instance for a given node
// name. It makes the assumption that he node name is resolvable.
// https://kubernetes.io/docs/concepts/architecture/nodes/#management
// So if the displayname doesn't match the nodename then:
//  1) Get the IP of the node name doing a reverse lookup and see if we can
//     find it.
//     NOTE(gbushell): I'm leaving the DNS lookup till later as the options
//     below fix/ the OKE issue.
//  2) See if the nodename is equal to the hostname label.
//  3) See if the nodename is an IP.
func (c *client) findInstanceByNodeNameIsVnic(nodeName string) (*baremetal.Instance, error) {
	var running []baremetal.Instance
	opts := &baremetal.ListVnicAttachmentsOptions{}
	for {
		vnicAttachments, err := c.ListVnicAttachments(c.compartmentOCID, opts)
		if err != nil {
			return nil, err
		}
		for _, attachment := range vnicAttachments.Attachments {
			if attachment.State == baremetal.ResourceAttached {
				vnic, err := c.GetVnic(attachment.VnicID)
				if err != nil {
					return nil, err
				}

				if vnic.PublicIPAddress == nodeName ||
					(vnic.HostnameLabel != "" && strings.HasPrefix(nodeName, vnic.HostnameLabel)) {
					instance, err := c.GetInstance(attachment.InstanceID)
					if err != nil {
						return nil, err
					}
					if instance.State == baremetal.ResourceRunning {
						running = append(running, *instance)
					}
				}
			}
		}
		if hasNextPage := SetNextPageOption(vnicAttachments.NextPage, &opts.ListOptions.PageListOptions); !hasNextPage {
			break
		}
	}

	count := len(running)
	switch {
	case count == 0:
		return nil, &SearchError{
			Err:      fmt.Sprintf("could not find instance for node name %q", nodeName),
			NotFound: true,
		}
	case count > 1:
		return nil, fmt.Errorf("expected one instance vnic ip/hostname %q but got %d", nodeName, count)
	}

	return &running[0], nil
}

// GetNodeAddressesForInstance gets the NodeAddress's of a given instance by
// OCID.
func (c *client) GetNodeAddressesForInstance(id string) ([]api.NodeAddress, error) {
	glog.V(4).Infof("getNodeAddressesForInstance(%q) called", id)
	if id == "" {
		return nil, fmt.Errorf("blank id passed to getNodeAddressesForInstance()")
	}

	vnics, err := c.GetAttachedVnicsForInstance(id)
	if err != nil {
		return nil, err
	}

	addresses := []api.NodeAddress{}
	for _, vnic := range vnics {
		addrs, err := c.extractNodeAddressesFromVnic(vnic)
		if err != nil {
			return nil, err
		}
		addresses = append(addresses, addrs...)
	}

	return addresses, nil
}

// extractNodeAddressesFromVnic extracts Kuberenetes NodeAddresses from a given
// Vnic.
// TODO: Remove fqdn lookup and then make a pure function.
func (c *client) extractNodeAddressesFromVnic(vnic *baremetal.Vnic) ([]api.NodeAddress, error) {
	glog.V(4).Infof("extractNodeAddressesFromVnic(%v) called", vnic)
	if vnic == nil {
		return nil, fmt.Errorf("nil Vnic passed to extractNodeAddressesFromVnic()")
	}

	addresses := []api.NodeAddress{}

	ip := net.ParseIP(vnic.PrivateIPAddress)
	if vnic.PrivateIPAddress != "" {
		if ip == nil {
			return nil, fmt.Errorf("instance has invalid private address: %q", vnic.PrivateIPAddress)
		}
		addresses = append(addresses, api.NodeAddress{Type: api.NodeInternalIP, Address: ip.String()})
	}

	// What if the instance isn't public?
	if vnic.PublicIPAddress != "" {
		ip = net.ParseIP(vnic.PublicIPAddress)
		if ip == nil {
			return nil, fmt.Errorf("instance has invalid public address: %q", vnic.PublicIPAddress)
		}
		addresses = append(addresses, api.NodeAddress{Type: api.NodeExternalIP, Address: ip.String()})
	}

	glog.V(4).Infof("NodeAddresses: %v ", addresses)

	return addresses, nil
}

// GetAttachedVnicsForInstance returns a slice of AVAILABLE Vnics for a
// given instance ocid.
func (c *client) GetAttachedVnicsForInstance(id string) ([]*baremetal.Vnic, error) {
	glog.V(4).Infof("getAttachedVnicsForInstance(%q) called", id)
	if id == "" {
		return nil, fmt.Errorf("blank instance id passed to getVincesForAttachedInstance()")
	}

	opts := &baremetal.ListVnicAttachmentsOptions{
		InstanceIDListOptions: baremetal.InstanceIDListOptions{InstanceID: id},
	}
	var vnics []*baremetal.Vnic
	for {
		r, err := c.ListVnicAttachments(c.compartmentOCID, opts)
		if err != nil {
			return nil, err
		}

		for _, att := range r.Attachments {
			if att.State != baremetal.ResourceAttached {
				continue
			}

			v, err := c.GetVnic(att.VnicID)
			if err != nil {
				return nil, err
			}

			if v.State != baremetal.ResourceAvailable {
				continue
			}
			vnics = append(vnics, v)
		}

		if hasNexPage := SetNextPageOption(r.NextPage, &opts.ListOptions.PageListOptions); !hasNexPage {
			break
		}
	}
	return vnics, nil
}

// f(n) = 1.25 * f(n - 1) | f(1) = 2
var backoff = wait.Backoff{
	Steps:    15,
	Duration: 2 * time.Second,
	Factor:   1.25,
	Jitter:   0.1,
}

// AwaitWorkRequest keeps polling a BMCS work request until it succeeds. If it
// does not succeeded after N retries then return an error.
func (c *client) AwaitWorkRequest(id string) (*baremetal.WorkRequest, error) {
	glog.V(4).Infof("Polling WorkRequest %q...", id)

	var wr *baremetal.WorkRequest
	opts := &baremetal.ClientRequestOptions{}
	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		twr, reqErr := c.GetWorkRequest(id, opts)
		if reqErr != nil {
			return false, reqErr
		}

		glog.V(4).Infof("WorkRequest %q state: '%s'", id, twr.State)

		switch twr.State {
		case baremetal.WorkRequestSucceeded:
			wr = twr
			return true, nil
		case baremetal.WorkRequestFailed:
			return false, fmt.Errorf("WorkRequest %q failed: %s", id, twr.Message)
		default:
			return false, nil
		}
	})
	return wr, err
}

// CreateAndAwaitLoadBalancer creates a load balancer and blocks until data is
// available or timeout is reached.
func (c *client) CreateAndAwaitLoadBalancer(name, shape string, subnets []string) (*baremetal.LoadBalancer, error) {
	opts := &baremetal.CreateLoadBalancerOptions{
		DisplayNameOptions: baremetal.DisplayNameOptions{
			DisplayName: name,
		},
	}

	req, err := c.CreateLoadBalancer(nil, nil, c.compartmentOCID, nil, shape, subnets, opts)
	if err != nil {
		return nil, err
	}

	result, err := c.AwaitWorkRequest(req)
	if err != nil {
		return nil, err
	}

	return c.GetLoadBalancer(result.LoadBalancerID, &baremetal.ClientRequestOptions{})
}

// GetLoadBalancerByName fetches a load balancer by its DisplayName.
func (c *client) GetLoadBalancerByName(name string) (*baremetal.LoadBalancer, error) {
	opts := &baremetal.ListOptions{}
	for {
		r, err := c.ListLoadBalancers(c.compartmentOCID, opts)
		if err != nil {
			return nil, err
		}
		for _, lb := range r.LoadBalancers {
			if lb.DisplayName == name {
				return &lb, nil
			}
		}
		if hasNexPage := SetNextPageOption(r.NextPage, &opts.PageListOptions); !hasNexPage {
			break
		}
	}
	return nil, &SearchError{
		Err:      fmt.Sprintf("could not find load balancer with name '%s'", name),
		NotFound: true,
	}
}

// CreateAndAwaitBackendSet creates the given BackendSet for the given
// LoadBalancer.
func (c *client) CreateAndAwaitBackendSet(lb *baremetal.LoadBalancer, bs baremetal.BackendSet) (*baremetal.BackendSet, error) {
	glog.V(2).Infof("Creating BackendSet '%s' for load balancer '%s'", bs.Name, lb.DisplayName)
	wr, err := c.CreateBackendSet(
		lb.ID,
		bs.Name,
		bs.Policy,
		bs.Backends,
		bs.HealthChecker,
		bs.SSLConfig,
		bs.SessionPersistenceConfig,
		nil)
	if err != nil {
		return nil, err
	}

	_, err = c.AwaitWorkRequest(wr)
	if err != nil {
		return nil, err
	}

	return c.GetBackendSet(lb.ID, bs.Name, &baremetal.ClientRequestOptions{})
}

// CreateAndAwaitListener creates the given Listener for the given LoadBalancer.
func (c *client) CreateAndAwaitListener(lb *baremetal.LoadBalancer, listener baremetal.Listener) error {
	glog.V(2).Infof("Creating Listener '%s' for load balancer '%s'", listener.Name, lb.DisplayName)
	wr, err := c.CreateListener(
		lb.ID,
		listener.Name,
		listener.DefaultBackendSetName,
		listener.Protocol,
		listener.Port,
		listener.SSLConfig,
		nil)
	if err != nil {
		return err
	}
	_, err = c.AwaitWorkRequest(wr)
	if err != nil {
		return err
	}
	return nil
}

// GetSubnetsForInternalIPs returns the deduplicated subnets in which the given
// internal IP addresses reside.
func (c *client) GetSubnetsForInternalIPs(ips []string) ([]*baremetal.Subnet, error) {
	ipSet := sets.NewString(ips...)

	opts := &baremetal.ListVnicAttachmentsOptions{}
	subnetOCIDs := sets.NewString()
	var subnets []*baremetal.Subnet
	for {
		r, err := c.ListVnicAttachments(c.compartmentOCID, nil)
		if err != nil {
			return nil, err
		}
		for _, attachment := range r.Attachments {
			if attachment.State == baremetal.ResourceAttached {
				vnic, err := c.GetVnic(attachment.VnicID)
				if err != nil {
					return nil, err
				}
				if vnic.PrivateIPAddress != "" && ipSet.Has(vnic.PrivateIPAddress) &&
					!subnetOCIDs.Has(vnic.SubnetID) {
					subnet, err := c.GetSubnet(vnic.SubnetID)
					if err != nil {
						return nil, err
					}
					subnets = append(subnets, subnet)
					subnetOCIDs.Insert(vnic.SubnetID)
				}
			}
		}
		if hasNexPage := SetNextPageOption(r.NextPage, &opts.PageListOptions); !hasNexPage {
			break
		}
	}
	return subnets, nil
}

// GetSubnets returns the Subnets corresponding to the given OCIDs.
func (c *client) GetSubnets(ocids []string) ([]*baremetal.Subnet, error) {
	var subnets []*baremetal.Subnet
	for _, ocid := range ocids {
		subnet, err := c.GetSubnet(ocid)
		if err != nil {
			return nil, err
		}
		subnets = append(subnets, subnet)
	}
	return subnets, nil
}

// GetDefaultSecurityList gets the default SecurityList for the given Subnet
// by assuming that the default SecurityList is always the oldest (as it is
// created automatically when the Subnet is created and cannot be deleted).
func (c *client) GetDefaultSecurityList(subnet *baremetal.Subnet) (*baremetal.SecurityList, error) {
	var lists []*baremetal.SecurityList
	for _, id := range subnet.SecurityListIDs {
		list, err := c.GetSecurityList(id)
		if err != nil {
			return nil, err
		}
		lists = append(lists, list)
	}

	if len(lists) < 1 {
		return nil, fmt.Errorf("no SecurityLists found for Subnet '%s'", subnet.ID)
	}

	sort.Slice(lists, func(i, j int) bool {
		return lists[i].TimeCreated.Before(lists[j].TimeCreated.Time)
	})
	return lists[0], nil
}