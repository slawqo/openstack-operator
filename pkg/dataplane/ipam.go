/*
Copyright 2023.

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

package deployment

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infranetworkv1 "github.com/openstack-k8s-operators/infra-operator/apis/network/v1beta1"
	condition "github.com/openstack-k8s-operators/lib-common/modules/common/condition"
	"github.com/openstack-k8s-operators/lib-common/modules/common/helper"
	"github.com/openstack-k8s-operators/lib-common/modules/common/util"
	dataplanev1 "github.com/openstack-k8s-operators/openstack-operator/apis/dataplane/v1beta1"
)

// DNSDetails struct for IPAM and DNS details of NodeSet
type DNSDetails struct {
	// IsReady has DNSData been reconciled
	IsReady bool
	// ServerAddresses holds a slice of DNS servers in the environment
	ServerAddresses []string
	// ClusterAddresses holds a slice of Kubernetes service ClusterIPs for the DNSMasq services
	ClusterAddresses []string
	// CtlplaneSearchDomain is the search domain provided by IPAM
	CtlplaneSearchDomain string
	// Hostnames is a map of hostnames provided by the NodeSet to the FQDNs
	Hostnames map[string]map[infranetworkv1.NetNameStr]string
	// AllIPs holds a map of all IP addresses per hostname.
	AllIPs map[string]map[infranetworkv1.NetNameStr]string
}

// checkDNSService checks if DNS is configured and ready
func checkDNSService(ctx context.Context, helper *helper.Helper,
	instance client.Object, dnsDetails *DNSDetails,
) error {
	dnsmasqList := &infranetworkv1.DNSMasqList{}
	listOpts := []client.ListOption{
		client.InNamespace(instance.GetNamespace()),
	}
	err := helper.GetClient().List(ctx, dnsmasqList, listOpts...)
	if err != nil {
		util.LogErrorForObject(helper, err, "Error listing dnsmasqs", instance)
		return err
	}
	if len(dnsmasqList.Items) > 1 {
		util.LogForObject(helper, "Only one DNS control plane service can exist", instance)
		err = errors.New(dataplanev1.NodeSetDNSDataMultipleDNSMasqErrorMessage)
		return err
	}
	if len(dnsmasqList.Items) == 0 {
		util.LogForObject(helper, "No DNS control plane service exists yet", instance)
		return nil
	}
	if !dnsmasqList.Items[0].IsReady() {
		util.LogForObject(helper, "DNS control plane service exists, but not ready yet ", instance)
		return nil
	}
	dnsDetails.ClusterAddresses = dnsmasqList.Items[0].Status.DNSClusterAddresses
	dnsDetails.ServerAddresses = dnsmasqList.Items[0].Status.DNSAddresses
	return nil
}

// createOrPatchDNSData builds the DNSData
func createOrPatchDNSData(ctx context.Context, helper *helper.Helper,
	instance *dataplanev1.OpenStackDataPlaneNodeSet,
	allIPSets map[string]infranetworkv1.IPSet, dnsDetails *DNSDetails,
) error {
	var allDNSRecords []infranetworkv1.DNSHost
	var ctlplaneSearchDomain string
	dnsDetails.Hostnames = map[string]map[infranetworkv1.NetNameStr]string{}
	dnsDetails.AllIPs = map[string]map[infranetworkv1.NetNameStr]string{}

	// Build DNSData CR
	// We need to sort the nodes here, else DNSData.Spec.Hosts would change
	// For every reconcile and it could create reconcile loops.
	nodes := instance.Spec.Nodes
	sortedNodeNames := make([]string, 0)
	for name := range instance.Spec.Nodes {
		sortedNodeNames = append(sortedNodeNames, name)
	}
	sort.Strings(sortedNodeNames)

	for _, nodeName := range sortedNodeNames {
		node := nodes[nodeName]
		var shortName string
		nets := node.Networks
		hostName := node.HostName

		dnsDetails.Hostnames[hostName] = map[infranetworkv1.NetNameStr]string{}
		dnsDetails.AllIPs[hostName] = map[infranetworkv1.NetNameStr]string{}

		shortName = strings.Split(hostName, ".")[0]
		if len(nets) == 0 {
			nets = instance.Spec.NodeTemplate.Networks
		}
		if len(nets) > 0 {
			// Get IPSet
			ipSet, ok := allIPSets[hostName]
			if ok {
				for _, res := range ipSet.Status.Reservation {
					var fqdnNames []string
					dnsRecord := infranetworkv1.DNSHost{}
					dnsRecord.IP = res.Address
					netLower := strings.ToLower(string(res.Network))
					fqdnName := strings.Join([]string{shortName, res.DNSDomain}, ".")
					if fqdnName != hostName {
						fqdnNames = append(fqdnNames, fqdnName)
						dnsDetails.Hostnames[hostName][infranetworkv1.NetNameStr(netLower)] = fqdnName
					}
					if dataplanev1.NodeHostNameIsFQDN(hostName) && netLower == CtlPlaneNetwork {
						fqdnNames = append(fqdnNames, hostName)
						dnsDetails.Hostnames[hostName][infranetworkv1.NetNameStr(netLower)] = hostName
					}
					dnsDetails.AllIPs[hostName][infranetworkv1.NetNameStr(netLower)] = res.Address
					dnsRecord.Hostnames = fqdnNames
					allDNSRecords = append(allDNSRecords, dnsRecord)
					// Adding only ctlplane domain for ansibleee.
					// TODO (rabi) This is not very efficient.
					if netLower == CtlPlaneNetwork && ctlplaneSearchDomain == "" {
						dnsDetails.CtlplaneSearchDomain = res.DNSDomain
					}
				}
			}
		}
	}
	dnsData := &infranetworkv1.DNSData{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: instance.Namespace,
			Name:      instance.Name,
		},
	}
	_, err := controllerutil.CreateOrPatch(ctx, helper.GetClient(), dnsData, func() error {
		dnsData.Spec.Hosts = allDNSRecords
		// TODO (rabi) DNSDataLabelSelectorValue can probably be
		// used from dnsmasq(?)
		dnsData.Spec.DNSDataLabelSelectorValue = "dnsdata"
		// Set controller reference to the DataPlaneNode object
		err := controllerutil.SetControllerReference(
			helper.GetBeforeObject(), dnsData, helper.GetScheme())
		return err
	})
	if err != nil {
		return err
	}
	return nil
}

// EnsureDNSData Ensures DNSData is created
func EnsureDNSData(ctx context.Context, helper *helper.Helper,
	instance *dataplanev1.OpenStackDataPlaneNodeSet,
	allIPSets map[string]infranetworkv1.IPSet,
) (dnsDetails *DNSDetails, err error) {
	dnsDetails = &DNSDetails{}
	// Verify dnsmasq CR exists
	err = checkDNSService(
		ctx, helper, instance, dnsDetails)

	if err != nil {
		instance.Status.Conditions.MarkFalse(
			dataplanev1.NodeSetDNSDataReadyCondition,
			condition.ErrorReason, condition.SeverityError,
			err.Error())
		return dnsDetails, err
	}
	if dnsDetails.ClusterAddresses == nil {
		instance.Status.Conditions.MarkFalse(
			dataplanev1.NodeSetDNSDataReadyCondition,
			condition.RequestedReason, condition.SeverityInfo,
			dataplanev1.NodeSetDNSDataReadyWaitingMessage)
		return dnsDetails, nil
	}

	// Create or Patch DNSData
	err = createOrPatchDNSData(
		ctx, helper, instance, allIPSets, dnsDetails)
	if err != nil {
		instance.Status.Conditions.MarkFalse(
			dataplanev1.NodeSetDNSDataReadyCondition,
			condition.ErrorReason, condition.SeverityError,
			dataplanev1.NodeSetDNSDataReadyErrorMessage)
		return dnsDetails, err
	}

	dnsData := &infranetworkv1.DNSData{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instance.Name,
			Namespace: instance.Namespace,
		},
	}
	key := client.ObjectKeyFromObject(dnsData)
	err = helper.GetClient().Get(ctx, key, dnsData)
	if err != nil {
		instance.Status.Conditions.MarkFalse(
			dataplanev1.NodeSetDNSDataReadyCondition,
			condition.ErrorReason, condition.SeverityError,
			dataplanev1.NodeSetDNSDataReadyErrorMessage)
		return dnsDetails, err
	}
	if !dnsData.IsReady() {
		util.LogForObject(helper, "DNSData not ready yet waiting", instance)
		instance.Status.Conditions.MarkFalse(
			dataplanev1.NodeSetDNSDataReadyCondition,
			condition.RequestedReason, condition.SeverityInfo,
			dataplanev1.NodeSetDNSDataReadyWaitingMessage)
		return dnsDetails, nil
	}

	instance.Status.Conditions.MarkTrue(
		dataplanev1.NodeSetDNSDataReadyCondition,
		dataplanev1.NodeSetDNSDataReadyMessage)
	dnsDetails.IsReady = true
	return dnsDetails, nil
}

// EnsureIPSets Creates the IPSets
func EnsureIPSets(ctx context.Context, helper *helper.Helper,
	instance *dataplanev1.OpenStackDataPlaneNodeSet,
) (map[string]infranetworkv1.IPSet, bool, error) {
	allIPSets, err := reserveIPs(ctx, helper, instance)
	if err != nil {
		instance.Status.Conditions.MarkFalse(
			dataplanev1.NodeSetIPReservationReadyCondition,
			condition.ErrorReason, condition.SeverityError,
			dataplanev1.NodeSetIPReservationReadyErrorMessage)
		return nil, false, err
	}

	for _, s := range allIPSets {
		if s.Status.Conditions.IsFalse(condition.ReadyCondition) {
			instance.Status.Conditions.MarkFalse(
				dataplanev1.NodeSetIPReservationReadyCondition,
				condition.RequestedReason, condition.SeverityInfo,
				dataplanev1.NodeSetIPReservationReadyWaitingMessage)
			return nil, false, nil
		}
	}
	instance.Status.Conditions.MarkTrue(
		dataplanev1.NodeSetIPReservationReadyCondition,
		dataplanev1.NodeSetIPReservationReadyMessage)
	return allIPSets, true, nil
}

// reserveIPs Reserves IPs by creating IPSets
func reserveIPs(ctx context.Context, helper *helper.Helper,
	instance *dataplanev1.OpenStackDataPlaneNodeSet,
) (map[string]infranetworkv1.IPSet, error) {
	// Verify NetConfig CRs exist
	netConfigList := &infranetworkv1.NetConfigList{}
	listOpts := []client.ListOption{
		client.InNamespace(instance.GetNamespace()),
	}
	err := helper.GetClient().List(ctx, netConfigList, listOpts...)
	if err != nil {
		return nil, err
	}
	if len(netConfigList.Items) == 0 {
		errMsg := "No NetConfig CR exists yet"
		util.LogForObject(helper, errMsg, instance)
		return nil, fmt.Errorf(errMsg)
	}

	allIPSets := make(map[string]infranetworkv1.IPSet)
	// CreateOrPatch IPSets
	for nodeName, node := range instance.Spec.Nodes {
		nets := node.Networks
		hostName := node.HostName
		if len(nets) == 0 {
			nets = instance.Spec.NodeTemplate.Networks
		}

		if len(nets) > 0 {
			ipSet := &infranetworkv1.IPSet{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: instance.Namespace,
					Name:      hostName,
				},
			}
			_, err := controllerutil.CreateOrPatch(ctx, helper.GetClient(), ipSet, func() error {
				ipSet.Spec.Networks = nets
				// Set controller reference to the DataPlaneNode object
				err := controllerutil.SetControllerReference(
					helper.GetBeforeObject(), ipSet, helper.GetScheme())
				return err
			})
			if err != nil {
				return nil, err
			}
			allIPSets[hostName] = *ipSet
		} else {
			msg := fmt.Sprintf("No Networks defined for node %s or template", nodeName)
			util.LogForObject(helper, msg, instance)
			return nil, fmt.Errorf(msg)
		}
	}

	return allIPSets, nil
}
