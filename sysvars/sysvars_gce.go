// Copyright 2017 The Cloudprober Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sysvars

import (
	"context"
	"fmt"
	"os"
	"strings"

	"cloud.google.com/go/compute/metadata"
	md "github.com/cloudprober/cloudprober/common/metadata"
	"github.com/cloudprober/cloudprober/logger"
	compute "google.golang.org/api/compute/v1"
)

// maxNICs is the number of NICs allowed on a VM. Used by addGceNicInfo.
var maxNICs = 8

// commonGCEGKEVars sets the variables that are available on both - GCE and GKE
// metadata. This list is based on
// https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity#gke_mds
func commonGCEGKEVars(vars map[string]string) error {
	metadataFuncs := map[string]func() (string, error){
		"project":     metadata.ProjectID,
		"project_id":  metadata.NumericProjectID,
		"instance_id": metadata.InstanceID,
	}

	for k, fn := range metadataFuncs {
		v, err := fn()
		if err != nil {
			return fmt.Errorf("sysvars_gce: error while getting %s from metadata: %v", k, err)
		}
		vars[k] = v
	}

	return nil
}

var gceVars = func(vars map[string]string, l *logger.Logger) (bool, error) {
	onGCE := metadata.OnGCE()
	if !onGCE {
		return false, nil
	}

	if err := commonGCEGKEVars(vars); err != nil {
		return onGCE, err
	}

	// If running on Kubernetes, don't bother setting rest of the variables. They
	// may or may not be available. Also, they may not be very relevant for
	// Kubernetes use case.
	if md.IsKubernetes() {
		// TODO(manugarg): See if we want to try setting variables on Kubernetes,
		// e.g. pod-name, cluster-name, cluser-location, etc.
		vars["namespace"] = md.KubernetesNamespace()
		return onGCE, nil
	}

	// If fetching instance name fails, we are on some restricted platform of
	// GCE. Exit sooner with no error.
	if instance, err := metadata.InstanceName(); err != nil {
		l.Warningf("Error getting instance name on GCE, using HOSTNAME environment variable: %v", err)
		vars["instance"] = os.Getenv("HOSTNAME")
		return onGCE, nil
	} else {
		vars["instance"] = instance
	}

	// Helper function we use below.
	getLastToken := func(value string) string {
		tokens := strings.Split(value, "/")
		return tokens[len(tokens)-1]
	}

	for _, k := range []string{
		"zone",
		"internal_ip",
		"external_ip",
		"instance_template",
		"machine_type",
	} {
		var v string
		var err error
		switch k {
		case "zone":
			v, err = metadata.Zone()
		case "internal_ip":
			v, err = metadata.InternalIP()
		case "external_ip":
			v, err = metadata.ExternalIP()
		case "instance_template":
			// `instance_template` may not be defined, depending on how cloudprober
			// was deployed. If an error is returned when fetching the metadata,
			// just fall back "undefined".
			v, err = metadata.InstanceAttributeValue("instance-template")
			if err != nil {
				l.Infof("No instance_template found. Defaulting to undefined.")
				v = "undefined"
				err = nil
			} else {
				v = getLastToken(v)
			}
		case "machine_type":
			v, err = metadata.Get("instance/machine-type")
			if err != nil {
				l.Infof("Could not fetch machine type. Defaulting to undefined.")
				v = "undefined"
				err = nil
			} else {
				v = getLastToken(v)
			}
		default:
			return onGCE, fmt.Errorf("sysvars_gce: unknown variable key %q", k)
		}
		if err != nil {
			return onGCE, fmt.Errorf("sysvars_gce: error while getting %s from metadata: %v", k, err)
		}
		vars[k] = v
	}

	zoneParts := strings.Split(vars["zone"], "-")
	vars["region"] = strings.Join(zoneParts[0:len(zoneParts)-1], "-")
	addGceNicInfo(vars, l)

	labels, err := labelsFromGCE(vars["project"], vars["zone"], vars["instance"])
	if err != nil {
		return onGCE, err
	}

	for k, v := range labels {
		// Adds GCE labels to the dictionary with a 'label_' prefix so they can be
		// referenced in the cfg file.
		vars["label_"+k] = v

	}
	return onGCE, nil
}

func labelsFromGCE(project, zone, instance string) (map[string]string, error) {
	ctx := context.Background()
	computeService, err := compute.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("error creating compute service to get instance labels: %v", err)
	}

	// Following call requires read-only access to compute API. We don't want to
	// fail initialization if that happens.
	i, err := computeService.Instances.Get(project, zone, instance).Context(ctx).Do()
	if err != nil {
		l.Warningf("sysvars_gce: Error while fetching the instance resource using GCE API: %v. Continuing without labels info.", err)
		return nil, nil
	}

	return i.Labels, nil
}

// addGceNicInfo adds nic information to vars.
// The following information is added for each nic.
// - Primary IP, if one is assigned to nic.
//	 If no primary IP is found, assume that NIC doesn't exist.
// - IPv6 IP, if one is assigned to nic.
//   If nic0 has IPv6 IP, then assign ip to key: "internal_ipv6_ip"
// - External IP, if one is assigned to nic.
// - An IP alias, if any IP alias ranges are assigned to nic.
//
// See the following document for more information on metadata.
// https://cloud.google.com/compute/docs/storing-retrieving-metadata
func addGceNicInfo(vars map[string]string, l *logger.Logger) {
	for i := 0; i < maxNICs; i++ {
		k := fmt.Sprintf("instance/network-interfaces/%v/ip", i)
		v, err := metadata.Get(k)
		// If there is no private IP for NIC, NIC doesn't exist.
		if err != nil {
			continue
		}
		vars[fmt.Sprintf("nic_%d_ip", i)] = v

		k = fmt.Sprintf("instance/network-interfaces/%v/ipv6s", i)
		v, err = metadata.Get(k)
		if err != nil {
			l.Debugf("VM does not have ipv6 ip on interface# %d", i)
		} else {
			v = strings.TrimSpace(v)
			vars[k] = v
			if i == 0 {
				vars["internal_ipv6_ip"] = v
			}
		}
	}
}
