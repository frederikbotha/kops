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

package model

import (
	"fmt"
	"sort"
	"time"

	"github.com/golang/glog"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/kops/pkg/apis/kops"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/awstasks"
)

const LoadBalancerDefaultIdleTimeout = 5 * time.Minute

// APILoadBalancerBuilder builds a LoadBalancer for accessing the API
type APILoadBalancerBuilder struct {
	*KopsModelContext
}

var _ fi.ModelBuilder = &APILoadBalancerBuilder{}

func (b *APILoadBalancerBuilder) Build(c *fi.ModelBuilderContext) error {
	// Configuration where an ELB fronts the API
	if !b.UseLoadBalancerForAPI() {
		return nil
	}

	lbSpec := b.Cluster.Spec.API.LoadBalancer
	if lbSpec == nil {
		// Skipping API ELB creation; not requested in Spec
		return nil
	}

	switch lbSpec.Type {
	case kops.LoadBalancerTypeInternal, kops.LoadBalancerTypePublic:
	// OK

	default:
		return fmt.Errorf("unhandled LoadBalancer type %q", lbSpec.Type)
	}

	// Compute the subnets - only one per zone, and then break ties based on chooseBestSubnetForELB
	var elbSubnets []*awstasks.Subnet
	{
		subnetsByZone := make(map[string][]*kops.ClusterSubnetSpec)
		for i := range b.Cluster.Spec.Subnets {
			subnet := &b.Cluster.Spec.Subnets[i]

			switch subnet.Type {
			case kops.SubnetTypePublic, kops.SubnetTypeUtility:
				if lbSpec.Type != kops.LoadBalancerTypePublic {
					continue
				}

			case kops.SubnetTypePrivate:
				if lbSpec.Type != kops.LoadBalancerTypeInternal {
					continue
				}

			default:
				return fmt.Errorf("subnet %q had unknown type %q", subnet.Name, subnet.Type)
			}

			subnetsByZone[subnet.Zone] = append(subnetsByZone[subnet.Zone], subnet)
		}

		for zone, subnets := range subnetsByZone {
			subnet := b.chooseBestSubnetForELB(zone, subnets)

			elbSubnets = append(elbSubnets, b.LinkToSubnet(subnet))
		}
	}

	var elb *awstasks.LoadBalancer
	{
		loadBalancerName := b.GetELBName32("api")

		idleTimeout := LoadBalancerDefaultIdleTimeout
		if lbSpec.IdleTimeoutSeconds != nil {
			idleTimeout = time.Second * time.Duration(*lbSpec.IdleTimeoutSeconds)
		}

		elb = &awstasks.LoadBalancer{
			Name:             s("api." + b.ClusterName()),
			LoadBalancerName: s(loadBalancerName),
			SecurityGroups: []*awstasks.SecurityGroup{
				b.LinkToELBSecurityGroup("api"),
			},
			Subnets: elbSubnets,
			Listeners: map[string]*awstasks.LoadBalancerListener{
				"443": {InstancePort: 443},
			},

			// Configure fast-recovery health-checks
			HealthCheck: &awstasks.LoadBalancerHealthCheck{
				Target:             s("TCP:443"),
				Timeout:            i64(5),
				Interval:           i64(10),
				HealthyThreshold:   i64(2),
				UnhealthyThreshold: i64(2),
			},

			ConnectionSettings: &awstasks.LoadBalancerConnectionSettings{
				IdleTimeout: i64(int64(idleTimeout.Seconds())),
			},
		}

		switch lbSpec.Type {
		case kops.LoadBalancerTypeInternal:
			elb.Scheme = s("internal")
		case kops.LoadBalancerTypePublic:
			elb.Scheme = nil
		default:
			return fmt.Errorf("unknown elb Type: %q", lbSpec.Type)
		}

		c.AddTask(elb)
	}

	// Create security group for API ELB
	{
		t := &awstasks.SecurityGroup{
			Name:             s(b.ELBSecurityGroupName("api")),
			VPC:              b.LinkToVPC(),
			Description:      s("Security group for api ELB"),
			RemoveExtraRules: []string{"port=443"},
		}
		c.AddTask(t)
	}

	// Allow traffic from ELB to egress freely
	{
		t := &awstasks.SecurityGroupRule{
			Name:          s("api-elb-egress"),
			SecurityGroup: b.LinkToELBSecurityGroup("api"),
			Egress:        fi.Bool(true),
			CIDR:          s("0.0.0.0/0"),
		}
		c.AddTask(t)
	}

	// Allow traffic into the ELB from KubernetesAPIAccess CIDRs
	{
		for _, cidr := range b.Cluster.Spec.KubernetesAPIAccess {
			t := &awstasks.SecurityGroupRule{
				Name:          s("https-api-elb-" + cidr),
				SecurityGroup: b.LinkToELBSecurityGroup("api"),
				CIDR:          s(cidr),
				FromPort:      i64(443),
				ToPort:        i64(443),
				Protocol:      s("tcp"),
			}
			c.AddTask(t)
		}
	}

	// Allow HTTPS to the master instances from the ELB
	{
		t := &awstasks.SecurityGroupRule{
			Name:          s("https-elb-to-master"),
			SecurityGroup: b.LinkToSecurityGroup(kops.InstanceGroupRoleMaster),
			SourceGroup:   b.LinkToELBSecurityGroup("api"),
			FromPort:      i64(443),
			ToPort:        i64(443),
			Protocol:      s("tcp"),
		}
		c.AddTask(t)
	}

	for _, ig := range b.MasterInstanceGroups() {
		t := &awstasks.LoadBalancerAttachment{
			Name: s("api-" + ig.ObjectMeta.Name),

			LoadBalancer:     b.LinkToELB("api"),
			AutoscalingGroup: b.LinkToAutoscalingGroup(ig),
		}

		c.AddTask(t)
	}

	return nil

}

type scoredSubnet struct {
	score  int
	subnet *kops.ClusterSubnetSpec
}

type ByScoreDescending []*scoredSubnet

func (a ByScoreDescending) Len() int      { return len(a) }
func (a ByScoreDescending) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a ByScoreDescending) Less(i, j int) bool {
	if a[i].score != a[j].score {
		// ! to sort highest score first
		return !(a[i].score < a[j].score)
	}
	// Use name to break ties consistently
	return a[i].subnet.Name < a[j].subnet.Name
}

// Choose between subnets in a zone.
// We have already applied the rules to match internal subnets to internal ELBs and vice-versa for public-facing ELBs.
// For internal ELBs: we prefer the master subnets
// For public facing ELBs: we prefer the utility subnets
func (b *APILoadBalancerBuilder) chooseBestSubnetForELB(zone string, subnets []*kops.ClusterSubnetSpec) *kops.ClusterSubnetSpec {
	if len(subnets) == 0 {
		return nil
	}
	if len(subnets) == 1 {
		return subnets[0]
	}

	migSubnets := sets.NewString()
	for _, ig := range b.MasterInstanceGroups() {
		for _, subnet := range ig.Spec.Subnets {
			migSubnets.Insert(subnet)
		}
	}

	var scoredSubnets []*scoredSubnet
	for _, subnet := range subnets {
		score := 0

		if migSubnets.Has(subnet.Name) {
			score += 1
		}

		if subnet.Type == kops.SubnetTypeUtility {
			score += 1
		}

		scoredSubnets = append(scoredSubnets, &scoredSubnet{
			score:  score,
			subnet: subnet,
		})
	}

	sort.Sort(ByScoreDescending(scoredSubnets))

	if scoredSubnets[0].score == scoredSubnets[1].score {
		glog.V(2).Infof("Making arbitrary choice between subnets in zone %q to attach to ELB (%q vs %q)", zone, scoredSubnets[0].subnet.Name, scoredSubnets[1].subnet.Name)
	}

	return scoredSubnets[0].subnet
}
