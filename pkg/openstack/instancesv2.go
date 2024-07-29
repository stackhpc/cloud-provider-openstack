/*
Copyright 2023 The Kubernetes Authors.

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

package openstack

import (
	"context"
	"fmt"
	sysos "os"
	"regexp"
	"strings"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/compute/v2/servers"
	v1 "k8s.io/api/core/v1"
	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/cloud-provider-openstack/pkg/client"
	"k8s.io/cloud-provider-openstack/pkg/metrics"
	"k8s.io/cloud-provider-openstack/pkg/util/errors"
	"k8s.io/klog/v2"
)

// InstancesV2 encapsulates an implementation of InstancesV2 for OpenStack.
type InstancesV2 struct {
	compute          *gophercloud.ServiceClient
	network          *gophercloud.ServiceClient
	region           string
	regionProviderID bool
	networkingOpts   NetworkingOpts
}

// InstancesV2 returns an implementation of InstancesV2 for OpenStack.
func (os *OpenStack) InstancesV2() (cloudprovider.InstancesV2, bool) {
	if !os.useV1Instances {
		return os.instancesv2()
	}
	return nil, false
}

func (os *OpenStack) instancesv2() (*InstancesV2, bool) {
	klog.V(4).Info("openstack.Instancesv2() called")

	compute, err := client.NewComputeV2(os.provider, os.epOpts)
	if err != nil {
		klog.Errorf("unable to access compute v2 API : %v", err)
		return nil, false
	}

	network, err := client.NewNetworkV2(os.provider, os.epOpts)
	if err != nil {
		klog.Errorf("unable to access network v2 API : %v", err)
		return nil, false
	}

	regionalProviderID := false
	if isRegionalProviderID := sysos.Getenv(RegionalProviderIDEnv); isRegionalProviderID == "true" {
		regionalProviderID = true
	}

	return &InstancesV2{
		compute:          compute,
		network:          network,
		region:           os.epOpts.Region,
		regionProviderID: regionalProviderID,
		networkingOpts:   os.networkingOpts,
	}, true
}

// InstanceExists indicates whether a given node exists according to the cloud provider
func (i *InstancesV2) InstanceExists(ctx context.Context, node *v1.Node) (bool, error) {
	_, err := i.getInstance(ctx, node)
	if err == cloudprovider.InstanceNotFound {
		klog.V(6).Infof("instance not found for node: %s", node.Name)
		return false, nil
	}

	if err != nil {
		return false, err
	}

	return true, nil
}

// InstanceShutdown returns true if the instance is shutdown according to the cloud provider.
func (i *InstancesV2) InstanceShutdown(ctx context.Context, node *v1.Node) (bool, error) {
	server, err := i.getInstance(ctx, node)
	if err != nil {
		return false, err
	}

	// SHUTOFF is the only state where we can detach volumes immediately
	if server.Status == instanceShutoff {
		return true, nil
	}

	return false, nil
}

func sanitizeLabel(input string) (string, error) {
	// Replace non-alphanumeric characters (except '-', '_', '.') with '-'
	reg := regexp.MustCompile(`[^-a-zA-Z0-9_.]+`)
	sanitized := reg.ReplaceAllString(input, "-")

	// Ensure the label starts and ends with an alphanumeric character
	sanitized = strings.Trim(sanitized, "-_.")

	// Ensure the label is not longer than 63 characters
	if len(sanitized) > 63 {
		sanitized = sanitized[:63]
	}

	// Convert to lowercase
	return strings.ToLower(sanitized), nil
}

// InstanceMetadata returns the instance's metadata.
func (i *InstancesV2) InstanceMetadata(ctx context.Context, node *v1.Node) (*cloudprovider.InstanceMetadata, error) {
	srv, err := i.getInstance(ctx, node)
	if err != nil {
		return nil, err
	}
	var server servers.Server
	if srv != nil {
		server = *srv
	}

	instanceType, err := srvInstanceType(i.compute, &server)
	if err != nil {
		return nil, err
	}

	ports, err := getAttachedPorts(i.network, server.ID)
	if err != nil {
		return nil, err
	}

	addresses, err := nodeAddresses(&server, ports, i.network, i.networkingOpts)
	if err != nil {
		return nil, err
	}

	availabilityZone, err := sanitizeLabel(server.AvailabilityZone)
	if err != nil {
		return nil, err
	}

	return &cloudprovider.InstanceMetadata{
		ProviderID:    i.makeInstanceID(&server),
		InstanceType:  instanceType,
		NodeAddresses: addresses,
		Zone:          availabilityZone,
		Region:        i.region,
	}, nil
}

func (i *InstancesV2) makeInstanceID(srv *servers.Server) string {
	if i.regionProviderID {
		return fmt.Sprintf("%s://%s/%s", ProviderName, i.region, srv.ID)
	}
	return fmt.Sprintf("%s:///%s", ProviderName, srv.ID)
}

func (i *InstancesV2) getInstance(ctx context.Context, node *v1.Node) (*servers.Server, error) {
	if node.Spec.ProviderID == "" {
		opt := servers.ListOpts{
			Name: fmt.Sprintf("^%s$", node.Name),
		}
		mc := metrics.NewMetricContext("server", "list")
		allPages, err := servers.List(i.compute, opt).AllPages(context.TODO())
		if mc.ObserveRequest(err) != nil {
			return nil, fmt.Errorf("error listing servers %v: %v", opt, err)
		}

		serverList, err := servers.ExtractServers(allPages)
		if err != nil {
			return nil, fmt.Errorf("error extracting servers from pages: %v", err)
		}
		if len(serverList) == 0 {
			return nil, cloudprovider.InstanceNotFound
		}
		if len(serverList) > 1 {
			return nil, fmt.Errorf("getInstance: multiple instances found")
		}
		return &serverList[0], nil
	}

	instanceID, instanceRegion, err := instanceIDFromProviderID(node.Spec.ProviderID)
	if err != nil {
		return nil, err
	}

	if instanceRegion != "" && instanceRegion != i.region {
		return nil, fmt.Errorf("ProviderID \"%s\" didn't match supported region \"%s\"", node.Spec.ProviderID, i.region)
	}

	mc := metrics.NewMetricContext("server", "get")
	server, err := servers.Get(context.TODO(), i.compute, instanceID).Extract()
	if mc.ObserveRequest(err) != nil {
		if errors.IsNotFound(err) {
			return nil, cloudprovider.InstanceNotFound
		}
		return nil, err
	}
	return server, nil
}
