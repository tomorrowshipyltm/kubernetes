// +build !providerless

/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package azure

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2019-12-01/compute"
	"github.com/Azure/azure-sdk-for-go/services/network/mgmt/2019-06-01/network"
	"github.com/Azure/go-autorest/autorest/to"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"
	azcache "k8s.io/legacy-cloud-providers/azure/cache"

	"k8s.io/component-base/featuregate"
	utilnet "k8s.io/utils/net"
)

const (
	// IPv6DualStack is here to avoid having to import features pkg
	// and violate import rules
	IPv6DualStack featuregate.Feature = "IPv6DualStack"

	loadBalancerMinimumPriority = 500
	loadBalancerMaximumPriority = 4096

	machineIDTemplate           = "/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/virtualMachines/%s"
	availabilitySetIDTemplate   = "/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/availabilitySets/%s"
	frontendIPConfigIDTemplate  = "/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/loadBalancers/%s/frontendIPConfigurations/%s"
	backendPoolIDTemplate       = "/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/loadBalancers/%s/backendAddressPools/%s"
	loadBalancerProbeIDTemplate = "/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/loadBalancers/%s/probes/%s"

	// InternalLoadBalancerNameSuffix is load balancer posfix
	InternalLoadBalancerNameSuffix = "-internal"

	// nodeLabelRole specifies the role of a node
	nodeLabelRole  = "kubernetes.io/role"
	nicFailedState = "Failed"

	storageAccountNameMaxLength   = 24
	frontendIPConfigNameMaxLength = 80
	loadBalancerRuleNameMaxLength = 80
)

var errNotInVMSet = errors.New("vm is not in the vmset")
var providerIDRE = regexp.MustCompile(`.*/subscriptions/(?:.*)/Microsoft.Compute/virtualMachines/(.+)$`)
var backendPoolIDRE = regexp.MustCompile(`^/subscriptions/(?:.*)/resourceGroups/(?:.*)/providers/Microsoft.Network/loadBalancers/(.+)/backendAddressPools/(?:.*)`)
var nicResourceGroupRE = regexp.MustCompile(`.*/subscriptions/(?:.*)/resourceGroups/(.+)/providers/Microsoft.Network/networkInterfaces/(?:.*)`)

// getStandardMachineID returns the full identifier of a virtual machine.
func (az *Cloud) getStandardMachineID(subscriptionID, resourceGroup, machineName string) string {
	return fmt.Sprintf(
		machineIDTemplate,
		subscriptionID,
		strings.ToLower(resourceGroup),
		machineName)
}

// returns the full identifier of an availabilitySet
func (az *Cloud) getAvailabilitySetID(resourceGroup, availabilitySetName string) string {
	return fmt.Sprintf(
		availabilitySetIDTemplate,
		az.SubscriptionID,
		resourceGroup,
		availabilitySetName)
}

// returns the full identifier of a loadbalancer frontendipconfiguration.
func (az *Cloud) getFrontendIPConfigID(lbName, rgName, fipConfigName string) string {
	return fmt.Sprintf(
		frontendIPConfigIDTemplate,
		az.getNetworkResourceSubscriptionID(),
		rgName,
		lbName,
		fipConfigName)
}

// returns the full identifier of a loadbalancer backendpool.
func (az *Cloud) getBackendPoolID(lbName, rgName, backendPoolName string) string {
	return fmt.Sprintf(
		backendPoolIDTemplate,
		az.getNetworkResourceSubscriptionID(),
		rgName,
		lbName,
		backendPoolName)
}

// returns the full identifier of a loadbalancer probe.
func (az *Cloud) getLoadBalancerProbeID(lbName, rgName, lbRuleName string) string {
	return fmt.Sprintf(
		loadBalancerProbeIDTemplate,
		az.getNetworkResourceSubscriptionID(),
		rgName,
		lbName,
		lbRuleName)
}

// getNetworkResourceSubscriptionID returns the subscription id which hosts network resources
func (az *Cloud) getNetworkResourceSubscriptionID() string {
	if az.Config.UsesNetworkResourceInDifferentTenant() {
		return az.NetworkResourceSubscriptionID
	}
	return az.SubscriptionID
}

func (az *Cloud) mapLoadBalancerNameToVMSet(lbName string, clusterName string) (vmSetName string) {
	vmSetName = strings.TrimSuffix(lbName, InternalLoadBalancerNameSuffix)
	if strings.EqualFold(clusterName, vmSetName) {
		vmSetName = az.VMSet.GetPrimaryVMSetName()
	}

	return vmSetName
}

// For a load balancer, all frontend ip should reference either a subnet or publicIpAddress.
// Thus Azure do not allow mixed type (public and internal) load balancer.
// So we'd have a separate name for internal load balancer.
// This would be the name for Azure LoadBalancer resource.
func (az *Cloud) getAzureLoadBalancerName(clusterName string, vmSetName string, isInternal bool) string {
	if az.LoadBalancerName != "" {
		clusterName = az.LoadBalancerName
	}
	lbNamePrefix := vmSetName
	if strings.EqualFold(vmSetName, az.VMSet.GetPrimaryVMSetName()) || az.useStandardLoadBalancer() {
		lbNamePrefix = clusterName
	}
	if isInternal {
		return fmt.Sprintf("%s%s", lbNamePrefix, InternalLoadBalancerNameSuffix)
	}
	return lbNamePrefix
}

// isMasterNode returns true if the node has a master role label.
// The master role is determined by looking for:
// * a kubernetes.io/role="master" label
func isMasterNode(node *v1.Node) bool {
	if val, ok := node.Labels[nodeLabelRole]; ok && val == "master" {
		return true
	}

	return false
}

// returns the deepest child's identifier from a full identifier string.
func getLastSegment(ID, separator string) (string, error) {
	parts := strings.Split(ID, separator)
	name := parts[len(parts)-1]
	if len(name) == 0 {
		return "", fmt.Errorf("resource name was missing from identifier")
	}

	return name, nil
}

// returns the equivalent LoadBalancerRule, SecurityRule and LoadBalancerProbe
// protocol types for the given Kubernetes protocol type.
func getProtocolsFromKubernetesProtocol(protocol v1.Protocol) (*network.TransportProtocol, *network.SecurityRuleProtocol, *network.ProbeProtocol, error) {
	var transportProto network.TransportProtocol
	var securityProto network.SecurityRuleProtocol
	var probeProto network.ProbeProtocol

	switch protocol {
	case v1.ProtocolTCP:
		transportProto = network.TransportProtocolTCP
		securityProto = network.SecurityRuleProtocolTCP
		probeProto = network.ProbeProtocolTCP
		return &transportProto, &securityProto, &probeProto, nil
	case v1.ProtocolUDP:
		transportProto = network.TransportProtocolUDP
		securityProto = network.SecurityRuleProtocolUDP
		return &transportProto, &securityProto, nil, nil
	default:
		return &transportProto, &securityProto, &probeProto, fmt.Errorf("only TCP and UDP are supported for Azure LoadBalancers")
	}

}

// This returns the full identifier of the primary NIC for the given VM.
func getPrimaryInterfaceID(machine compute.VirtualMachine) (string, error) {
	if len(*machine.NetworkProfile.NetworkInterfaces) == 1 {
		return *(*machine.NetworkProfile.NetworkInterfaces)[0].ID, nil
	}

	for _, ref := range *machine.NetworkProfile.NetworkInterfaces {
		if *ref.Primary {
			return *ref.ID, nil
		}
	}

	return "", fmt.Errorf("failed to find a primary nic for the vm. vmname=%q", *machine.Name)
}

func getPrimaryIPConfig(nic network.Interface) (*network.InterfaceIPConfiguration, error) {
	if nic.IPConfigurations == nil {
		return nil, fmt.Errorf("nic.IPConfigurations for nic (nicname=%q) is nil", *nic.Name)
	}

	if len(*nic.IPConfigurations) == 1 {
		return &((*nic.IPConfigurations)[0]), nil
	}

	for _, ref := range *nic.IPConfigurations {
		if *ref.Primary {
			return &ref, nil
		}
	}

	return nil, fmt.Errorf("failed to determine the primary ipconfig. nicname=%q", *nic.Name)
}

// returns first ip configuration on a nic by family
func getIPConfigByIPFamily(nic network.Interface, IPv6 bool) (*network.InterfaceIPConfiguration, error) {
	if nic.IPConfigurations == nil {
		return nil, fmt.Errorf("nic.IPConfigurations for nic (nicname=%q) is nil", *nic.Name)
	}

	var ipVersion network.IPVersion
	if IPv6 {
		ipVersion = network.IPv6
	} else {
		ipVersion = network.IPv4
	}
	for _, ref := range *nic.IPConfigurations {
		if ref.PrivateIPAddress != nil && ref.PrivateIPAddressVersion == ipVersion {
			return &ref, nil
		}
	}
	return nil, fmt.Errorf("failed to determine the ipconfig(IPv6=%v). nicname=%q", IPv6, *nic.Name)
}

func isInternalLoadBalancer(lb *network.LoadBalancer) bool {
	return strings.HasSuffix(*lb.Name, InternalLoadBalancerNameSuffix)
}

// getBackendPoolName the LB BackendPool name for a service.
// to ensure backword and forward compat:
// SingleStack -v4 (pre v1.16) => BackendPool name == clusterName
// SingleStack -v6 => BackendPool name == <clusterName>-IPv6 (all cluster bootstrap uses this name)
// DualStack
//	=> IPv4 BackendPool name == clusterName
//  => IPv6 BackendPool name == <clusterName>-IPv6
// This means:
// clusters moving from IPv4 to dualstack will require no changes
// clusters moving from IPv6 to dualstack will require no changes as the IPv4 backend pool will created with <clusterName>
func getBackendPoolName(clusterName string, service *v1.Service) string {
	IPv6 := utilnet.IsIPv6String(service.Spec.ClusterIP)
	if IPv6 {
		return fmt.Sprintf("%v-IPv6", clusterName)
	}

	return clusterName
}

func (az *Cloud) getLoadBalancerRuleName(service *v1.Service, protocol v1.Protocol, port int32) string {
	prefix := az.getRulePrefix(service)
	ruleName := fmt.Sprintf("%s-%s-%d", prefix, protocol, port)
	subnet := subnet(service)
	if subnet == nil {
		return ruleName
	}

	// Load balancer rule name must be less or equal to 80 characters, so excluding the hyphen two segments cannot exceed 79
	subnetSegment := *subnet
	if len(ruleName)+len(subnetSegment)+1 > loadBalancerRuleNameMaxLength {
		subnetSegment = subnetSegment[:loadBalancerRuleNameMaxLength-len(ruleName)-1]
	}

	return fmt.Sprintf("%s-%s-%s-%d", prefix, subnetSegment, protocol, port)
}

func (az *Cloud) getSecurityRuleName(service *v1.Service, port v1.ServicePort, sourceAddrPrefix string) string {
	if useSharedSecurityRule(service) {
		safePrefix := strings.Replace(sourceAddrPrefix, "/", "_", -1)
		return fmt.Sprintf("shared-%s-%d-%s", port.Protocol, port.Port, safePrefix)
	}
	safePrefix := strings.Replace(sourceAddrPrefix, "/", "_", -1)
	rulePrefix := az.getRulePrefix(service)
	return fmt.Sprintf("%s-%s-%d-%s", rulePrefix, port.Protocol, port.Port, safePrefix)
}

// This returns a human-readable version of the Service used to tag some resources.
// This is only used for human-readable convenience, and not to filter.
func getServiceName(service *v1.Service) string {
	return fmt.Sprintf("%s/%s", service.Namespace, service.Name)
}

// This returns a prefix for loadbalancer/security rules.
func (az *Cloud) getRulePrefix(service *v1.Service) string {
	return az.GetLoadBalancerName(context.TODO(), "", service)
}

func (az *Cloud) getPublicIPName(clusterName string, service *v1.Service) string {
	return fmt.Sprintf("%s-%s", clusterName, az.GetLoadBalancerName(context.TODO(), clusterName, service))
}

func (az *Cloud) serviceOwnsRule(service *v1.Service, rule string) bool {
	prefix := az.getRulePrefix(service)
	return strings.HasPrefix(strings.ToUpper(rule), strings.ToUpper(prefix))
}

func (az *Cloud) serviceOwnsFrontendIP(fip network.FrontendIPConfiguration, service *v1.Service) bool {
	baseName := az.GetLoadBalancerName(context.TODO(), "", service)
	return strings.HasPrefix(*fip.Name, baseName)
}

func (az *Cloud) getFrontendIPConfigName(service *v1.Service) string {
	baseName := az.GetLoadBalancerName(context.TODO(), "", service)
	subnetName := subnet(service)
	if subnetName != nil {
		ipcName := fmt.Sprintf("%s-%s", baseName, *subnetName)

		// Azure lb front end configuration name must not exceed 80 characters
		if len(ipcName) > frontendIPConfigNameMaxLength {
			ipcName = ipcName[:frontendIPConfigNameMaxLength]
		}
		return ipcName
	}
	return baseName
}

// This returns the next available rule priority level for a given set of security rules.
func getNextAvailablePriority(rules []network.SecurityRule) (int32, error) {
	var smallest int32 = loadBalancerMinimumPriority
	var spread int32 = 1

outer:
	for smallest < loadBalancerMaximumPriority {
		for _, rule := range rules {
			if *rule.Priority == smallest {
				smallest += spread
				continue outer
			}
		}
		// no one else had it
		return smallest, nil
	}

	return -1, fmt.Errorf("securityGroup priorities are exhausted")
}

var polyTable = crc32.MakeTable(crc32.Koopman)

//MakeCRC32 : convert string to CRC32 format
func MakeCRC32(str string) string {
	crc := crc32.New(polyTable)
	crc.Write([]byte(str))
	hash := crc.Sum32()
	return strconv.FormatUint(uint64(hash), 10)
}

// availabilitySet implements VMSet interface for Azure availability sets.
type availabilitySet struct {
	*Cloud
}

// newStandardSet creates a new availabilitySet.
func newAvailabilitySet(az *Cloud) VMSet {
	return &availabilitySet{
		Cloud: az,
	}
}

// GetInstanceIDByNodeName gets the cloud provider ID by node name.
// It must return ("", cloudprovider.InstanceNotFound) if the instance does
// not exist or is no longer running.
func (as *availabilitySet) GetInstanceIDByNodeName(name string) (string, error) {
	var machine compute.VirtualMachine
	var err error

	machine, err = as.getVirtualMachine(types.NodeName(name), azcache.CacheReadTypeUnsafe)
	if err == cloudprovider.InstanceNotFound {
		return "", cloudprovider.InstanceNotFound
	}
	if err != nil {
		if as.CloudProviderBackoff {
			klog.V(2).Infof("GetInstanceIDByNodeName(%s) backing off", name)
			machine, err = as.GetVirtualMachineWithRetry(types.NodeName(name), azcache.CacheReadTypeUnsafe)
			if err != nil {
				klog.V(2).Infof("GetInstanceIDByNodeName(%s) abort backoff", name)
				return "", err
			}
		} else {
			return "", err
		}
	}

	resourceID := *machine.ID
	convertedResourceID, err := convertResourceGroupNameToLower(resourceID)
	if err != nil {
		klog.Errorf("convertResourceGroupNameToLower failed with error: %v", err)
		return "", err
	}
	return convertedResourceID, nil
}

// GetPowerStatusByNodeName returns the power state of the specified node.
func (as *availabilitySet) GetPowerStatusByNodeName(name string) (powerState string, err error) {
	vm, err := as.getVirtualMachine(types.NodeName(name), azcache.CacheReadTypeDefault)
	if err != nil {
		return powerState, err
	}

	if vm.InstanceView != nil && vm.InstanceView.Statuses != nil {
		statuses := *vm.InstanceView.Statuses
		for _, status := range statuses {
			state := to.String(status.Code)
			if strings.HasPrefix(state, vmPowerStatePrefix) {
				return strings.TrimPrefix(state, vmPowerStatePrefix), nil
			}
		}
	}

	// vm.InstanceView or vm.InstanceView.Statuses are nil when the VM is under deleting.
	klog.V(3).Infof("InstanceView for node %q is nil, assuming it's stopped", name)
	return vmPowerStateStopped, nil
}

// GetProvisioningStateByNodeName returns the provisioningState for the specified node.
func (as *availabilitySet) GetProvisioningStateByNodeName(name string) (provisioningState string, err error) {
	vm, err := as.getVirtualMachine(types.NodeName(name), azcache.CacheReadTypeDefault)
	if err != nil {
		return provisioningState, err
	}

	if vm.VirtualMachineProperties == nil || vm.VirtualMachineProperties.ProvisioningState == nil {
		return provisioningState, nil
	}

	return to.String(vm.VirtualMachineProperties.ProvisioningState), nil
}

// GetNodeNameByProviderID gets the node name by provider ID.
func (as *availabilitySet) GetNodeNameByProviderID(providerID string) (types.NodeName, error) {
	// NodeName is part of providerID for standard instances.
	matches := providerIDRE.FindStringSubmatch(providerID)
	if len(matches) != 2 {
		return "", errors.New("error splitting providerID")
	}

	return types.NodeName(matches[1]), nil
}

// GetInstanceTypeByNodeName gets the instance type by node name.
func (as *availabilitySet) GetInstanceTypeByNodeName(name string) (string, error) {
	machine, err := as.getVirtualMachine(types.NodeName(name), azcache.CacheReadTypeUnsafe)
	if err != nil {
		klog.Errorf("as.GetInstanceTypeByNodeName(%s) failed: as.getVirtualMachine(%s) err=%v", name, name, err)
		return "", err
	}

	return string(machine.HardwareProfile.VMSize), nil
}

// GetZoneByNodeName gets availability zone for the specified node. If the node is not running
// with availability zone, then it returns fault domain.
func (as *availabilitySet) GetZoneByNodeName(name string) (cloudprovider.Zone, error) {
	vm, err := as.getVirtualMachine(types.NodeName(name), azcache.CacheReadTypeUnsafe)
	if err != nil {
		return cloudprovider.Zone{}, err
	}

	var failureDomain string
	if vm.Zones != nil && len(*vm.Zones) > 0 {
		// Get availability zone for the node.
		zones := *vm.Zones
		zoneID, err := strconv.Atoi(zones[0])
		if err != nil {
			return cloudprovider.Zone{}, fmt.Errorf("failed to parse zone %q: %v", zones, err)
		}

		failureDomain = as.makeZone(to.String(vm.Location), zoneID)
	} else {
		// Availability zone is not used for the node, falling back to fault domain.
		failureDomain = strconv.Itoa(int(to.Int32(vm.VirtualMachineProperties.InstanceView.PlatformFaultDomain)))
	}

	zone := cloudprovider.Zone{
		FailureDomain: strings.ToLower(failureDomain),
		Region:        strings.ToLower(to.String(vm.Location)),
	}
	return zone, nil
}

// GetPrimaryVMSetName returns the VM set name depending on the configured vmType.
// It returns config.PrimaryScaleSetName for vmss and config.PrimaryAvailabilitySetName for standard vmType.
func (as *availabilitySet) GetPrimaryVMSetName() string {
	return as.Config.PrimaryAvailabilitySetName
}

// GetIPByNodeName gets machine private IP and public IP by node name.
func (as *availabilitySet) GetIPByNodeName(name string) (string, string, error) {
	nic, err := as.GetPrimaryInterface(name)
	if err != nil {
		return "", "", err
	}

	ipConfig, err := getPrimaryIPConfig(nic)
	if err != nil {
		klog.Errorf("as.GetIPByNodeName(%s) failed: getPrimaryIPConfig(%v), err=%v", name, nic, err)
		return "", "", err
	}

	privateIP := *ipConfig.PrivateIPAddress
	publicIP := ""
	if ipConfig.PublicIPAddress != nil && ipConfig.PublicIPAddress.ID != nil {
		pipID := *ipConfig.PublicIPAddress.ID
		pipName, err := getLastSegment(pipID, "/")
		if err != nil {
			return "", "", fmt.Errorf("failed to publicIP name for node %q with pipID %q", name, pipID)
		}
		pip, existsPip, err := as.getPublicIPAddress(as.ResourceGroup, pipName)
		if err != nil {
			return "", "", err
		}
		if existsPip {
			publicIP = *pip.IPAddress
		}
	}

	return privateIP, publicIP, nil
}

// returns a list of private ips assigned to node
// TODO (khenidak): This should read all nics, not just the primary
// allowing users to split ipv4/v6 on multiple nics
func (as *availabilitySet) GetPrivateIPsByNodeName(name string) ([]string, error) {
	ips := make([]string, 0)
	nic, err := as.GetPrimaryInterface(name)
	if err != nil {
		return ips, err
	}

	if nic.IPConfigurations == nil {
		return ips, fmt.Errorf("nic.IPConfigurations for nic (nicname=%q) is nil", *nic.Name)
	}

	for _, ipConfig := range *(nic.IPConfigurations) {
		if ipConfig.PrivateIPAddress != nil {
			ips = append(ips, *(ipConfig.PrivateIPAddress))
		}
	}

	return ips, nil
}

// getAgentPoolAvailabiliySets lists the virtual machines for the resource group and then builds
// a list of availability sets that match the nodes available to k8s.
func (as *availabilitySet) getAgentPoolAvailabiliySets(nodes []*v1.Node) (agentPoolAvailabilitySets *[]string, err error) {
	vms, err := as.ListVirtualMachines(as.ResourceGroup)
	if err != nil {
		klog.Errorf("as.getNodeAvailabilitySet - ListVirtualMachines failed, err=%v", err)
		return nil, err
	}
	vmNameToAvailabilitySetID := make(map[string]string, len(vms))
	for vmx := range vms {
		vm := vms[vmx]
		if vm.AvailabilitySet != nil {
			vmNameToAvailabilitySetID[*vm.Name] = *vm.AvailabilitySet.ID
		}
	}
	availabilitySetIDs := sets.NewString()
	agentPoolAvailabilitySets = &[]string{}
	for nx := range nodes {
		nodeName := (*nodes[nx]).Name
		if isMasterNode(nodes[nx]) {
			continue
		}
		asID, ok := vmNameToAvailabilitySetID[nodeName]
		if !ok {
			klog.Errorf("as.getNodeAvailabilitySet - Node(%s) has no availability sets", nodeName)
			return nil, fmt.Errorf("Node (%s) - has no availability sets", nodeName)
		}
		if availabilitySetIDs.Has(asID) {
			// already added in the list
			continue
		}
		asName, err := getLastSegment(asID, "/")
		if err != nil {
			klog.Errorf("as.getNodeAvailabilitySet - Node (%s)- getLastSegment(%s), err=%v", nodeName, asID, err)
			return nil, err
		}
		// AvailabilitySet ID is currently upper cased in a indeterministic way
		// We want to keep it lower case, before the ID get fixed
		asName = strings.ToLower(asName)

		*agentPoolAvailabilitySets = append(*agentPoolAvailabilitySets, asName)
	}

	return agentPoolAvailabilitySets, nil
}

// GetVMSetNames selects all possible availability sets or scale sets
// (depending vmType configured) for service load balancer, if the service has
// no loadbalancer mode annotaion returns the primary VMSet. If service annotation
// for loadbalancer exists then return the eligible VMSet.
func (as *availabilitySet) GetVMSetNames(service *v1.Service, nodes []*v1.Node) (availabilitySetNames *[]string, err error) {
	hasMode, isAuto, serviceAvailabilitySetNames := getServiceLoadBalancerMode(service)
	if !hasMode {
		// no mode specified in service annotation default to PrimaryAvailabilitySetName
		availabilitySetNames = &[]string{as.Config.PrimaryAvailabilitySetName}
		return availabilitySetNames, nil
	}
	availabilitySetNames, err = as.getAgentPoolAvailabiliySets(nodes)
	if err != nil {
		klog.Errorf("as.GetVMSetNames - getAgentPoolAvailabiliySets failed err=(%v)", err)
		return nil, err
	}
	if len(*availabilitySetNames) == 0 {
		klog.Errorf("as.GetVMSetNames - No availability sets found for nodes in the cluster, node count(%d)", len(nodes))
		return nil, fmt.Errorf("No availability sets found for nodes, node count(%d)", len(nodes))
	}
	// sort the list to have deterministic selection
	sort.Strings(*availabilitySetNames)
	if !isAuto {
		if serviceAvailabilitySetNames == nil || len(serviceAvailabilitySetNames) == 0 {
			return nil, fmt.Errorf("service annotation for LoadBalancerMode is empty, it should have __auto__ or availability sets value")
		}
		// validate availability set exists
		var found bool
		for sasx := range serviceAvailabilitySetNames {
			for asx := range *availabilitySetNames {
				if strings.EqualFold((*availabilitySetNames)[asx], serviceAvailabilitySetNames[sasx]) {
					found = true
					serviceAvailabilitySetNames[sasx] = (*availabilitySetNames)[asx]
					break
				}
			}
			if !found {
				klog.Errorf("as.GetVMSetNames - Availability set (%s) in service annotation not found", serviceAvailabilitySetNames[sasx])
				return nil, fmt.Errorf("availability set (%s) - not found", serviceAvailabilitySetNames[sasx])
			}
		}
		availabilitySetNames = &serviceAvailabilitySetNames
	}

	return availabilitySetNames, nil
}

// GetPrimaryInterface gets machine primary network interface by node name.
func (as *availabilitySet) GetPrimaryInterface(nodeName string) (network.Interface, error) {
	return as.getPrimaryInterfaceWithVMSet(nodeName, "")
}

// extractResourceGroupByNicID extracts the resource group name by nicID.
func extractResourceGroupByNicID(nicID string) (string, error) {
	matches := nicResourceGroupRE.FindStringSubmatch(nicID)
	if len(matches) != 2 {
		return "", fmt.Errorf("error of extracting resourceGroup from nicID %q", nicID)
	}

	return matches[1], nil
}

// getPrimaryInterfaceWithVMSet gets machine primary network interface by node name and vmSet.
func (as *availabilitySet) getPrimaryInterfaceWithVMSet(nodeName, vmSetName string) (network.Interface, error) {
	var machine compute.VirtualMachine

	machine, err := as.GetVirtualMachineWithRetry(types.NodeName(nodeName), azcache.CacheReadTypeDefault)
	if err != nil {
		klog.V(2).Infof("GetPrimaryInterface(%s, %s) abort backoff", nodeName, vmSetName)
		return network.Interface{}, err
	}

	primaryNicID, err := getPrimaryInterfaceID(machine)
	if err != nil {
		return network.Interface{}, err
	}
	nicName, err := getLastSegment(primaryNicID, "/")
	if err != nil {
		return network.Interface{}, err
	}
	nodeResourceGroup, err := as.GetNodeResourceGroup(nodeName)
	if err != nil {
		return network.Interface{}, err
	}

	// Check availability set name. Note that vmSetName is empty string when getting
	// the Node's IP address. While vmSetName is not empty, it should be checked with
	// Node's real availability set name:
	// - For basic SKU load balancer, errNotInVMSet should be returned if the node's
	//   availability set is mismatched with vmSetName.
	// - For standard SKU load balancer, backend could belong to multiple VMAS, so we
	//   don't check vmSet for it.
	if vmSetName != "" && !as.useStandardLoadBalancer() {
		expectedAvailabilitySetName := as.getAvailabilitySetID(nodeResourceGroup, vmSetName)
		if machine.AvailabilitySet == nil || !strings.EqualFold(*machine.AvailabilitySet.ID, expectedAvailabilitySetName) {
			klog.V(3).Infof(
				"GetPrimaryInterface: nic (%s) is not in the availabilitySet(%s)", nicName, vmSetName)
			return network.Interface{}, errNotInVMSet
		}
	}

	nicResourceGroup, err := extractResourceGroupByNicID(primaryNicID)
	if err != nil {
		return network.Interface{}, err
	}

	ctx, cancel := getContextWithCancel()
	defer cancel()
	nic, rerr := as.InterfacesClient.Get(ctx, nicResourceGroup, nicName, "")
	if rerr != nil {
		return network.Interface{}, rerr.Error()
	}

	return nic, nil
}

// EnsureHostInPool ensures the given VM's Primary NIC's Primary IP Configuration is
// participating in the specified LoadBalancer Backend Pool.
func (as *availabilitySet) EnsureHostInPool(service *v1.Service, nodeName types.NodeName, backendPoolID string, vmSetName string, isInternal bool) (string, string, string, *compute.VirtualMachineScaleSetVM, error) {
	vmName := mapNodeNameToVMName(nodeName)
	serviceName := getServiceName(service)
	nic, err := as.getPrimaryInterfaceWithVMSet(vmName, vmSetName)
	if err != nil {
		if err == errNotInVMSet {
			klog.V(3).Infof("EnsureHostInPool skips node %s because it is not in the vmSet %s", nodeName, vmSetName)
			return "", "", "", nil, nil
		}

		klog.Errorf("error: az.EnsureHostInPool(%s), az.VMSet.GetPrimaryInterface.Get(%s, %s), err=%v", nodeName, vmName, vmSetName, err)
		return "", "", "", nil, err
	}

	if nic.ProvisioningState != nil && *nic.ProvisioningState == nicFailedState {
		klog.Warningf("EnsureHostInPool skips node %s because its primary nic %s is in Failed state", nodeName, *nic.Name)
		return "", "", "", nil, nil
	}

	var primaryIPConfig *network.InterfaceIPConfiguration
	ipv6 := utilnet.IsIPv6String(service.Spec.ClusterIP)
	if !as.Cloud.ipv6DualStackEnabled && !ipv6 {
		primaryIPConfig, err = getPrimaryIPConfig(nic)
		if err != nil {
			return "", "", "", nil, err
		}
	} else {
		primaryIPConfig, err = getIPConfigByIPFamily(nic, ipv6)
		if err != nil {
			return "", "", "", nil, err
		}
	}

	foundPool := false
	newBackendPools := []network.BackendAddressPool{}
	if primaryIPConfig.LoadBalancerBackendAddressPools != nil {
		newBackendPools = *primaryIPConfig.LoadBalancerBackendAddressPools
	}
	for _, existingPool := range newBackendPools {
		if strings.EqualFold(backendPoolID, *existingPool.ID) {
			foundPool = true
			break
		}
	}
	if !foundPool {
		if as.useStandardLoadBalancer() && len(newBackendPools) > 0 {
			// Although standard load balancer supports backends from multiple availability
			// sets, the same network interface couldn't be added to more than one load balancer of
			// the same type. Omit those nodes (e.g. masters) so Azure ARM won't complain
			// about this.
			newBackendPoolsIDs := make([]string, 0, len(newBackendPools))
			for _, pool := range newBackendPools {
				if pool.ID != nil {
					newBackendPoolsIDs = append(newBackendPoolsIDs, *pool.ID)
				}
			}
			isSameLB, oldLBName, err := isBackendPoolOnSameLB(backendPoolID, newBackendPoolsIDs)
			if err != nil {
				return "", "", "", nil, err
			}
			if !isSameLB {
				klog.V(4).Infof("Node %q has already been added to LB %q, omit adding it to a new one", nodeName, oldLBName)
				return "", "", "", nil, nil
			}
		}

		newBackendPools = append(newBackendPools,
			network.BackendAddressPool{
				ID: to.StringPtr(backendPoolID),
			})

		primaryIPConfig.LoadBalancerBackendAddressPools = &newBackendPools

		nicName := *nic.Name
		klog.V(3).Infof("nicupdate(%s): nic(%s) - updating", serviceName, nicName)
		err := as.CreateOrUpdateInterface(service, nic)
		if err != nil {
			return "", "", "", nil, err
		}
	}
	return "", "", "", nil, nil
}

// EnsureHostsInPool ensures the given Node's primary IP configurations are
// participating in the specified LoadBalancer Backend Pool.
func (as *availabilitySet) EnsureHostsInPool(service *v1.Service, nodes []*v1.Node, backendPoolID string, vmSetName string, isInternal bool) error {
	hostUpdates := make([]func() error, 0, len(nodes))
	for _, node := range nodes {
		localNodeName := node.Name
		if as.useStandardLoadBalancer() && as.excludeMasterNodesFromStandardLB() && isMasterNode(node) {
			klog.V(4).Infof("Excluding master node %q from load balancer backendpool %q", localNodeName, backendPoolID)
			continue
		}

		if as.ShouldNodeExcludedFromLoadBalancer(node) {
			klog.V(4).Infof("Excluding unmanaged/external-resource-group node %q", localNodeName)
			continue
		}

		f := func() error {
			_, _, _, _, err := as.EnsureHostInPool(service, types.NodeName(localNodeName), backendPoolID, vmSetName, isInternal)
			if err != nil {
				return fmt.Errorf("ensure(%s): backendPoolID(%s) - failed to ensure host in pool: %q", getServiceName(service), backendPoolID, err)
			}
			return nil
		}
		hostUpdates = append(hostUpdates, f)
	}

	errs := utilerrors.AggregateGoroutines(hostUpdates...)
	if errs != nil {
		return utilerrors.Flatten(errs)
	}

	return nil
}

// EnsureBackendPoolDeleted ensures the loadBalancer backendAddressPools deleted from the specified nodes.
func (as *availabilitySet) EnsureBackendPoolDeleted(service *v1.Service, backendPoolID, vmSetName string, backendAddressPools *[]network.BackendAddressPool) error {
	// Do nothing for availability set.
	return nil
}

// get a storage account by UUID
func generateStorageAccountName(accountNamePrefix string) string {
	uniqueID := strings.Replace(string(uuid.NewUUID()), "-", "", -1)
	accountName := strings.ToLower(accountNamePrefix + uniqueID)
	if len(accountName) > storageAccountNameMaxLength {
		return accountName[:storageAccountNameMaxLength-1]
	}
	return accountName
}
