// Copyright 2025 Red Hat, Inc.
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
//
// This file was developed with the assistance of AI tools. While AI provided
// suggestions and code generation support, all final code, design decisions,
// and implementation remain the responsibility of human developers.

package aws

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
)

// Route53Client wraps the AWS Route53 client with helper methods
type Route53Client struct {
	client *route53.Client
}

// NewRoute53Client creates a new Route53 client with AWS SDK v2
func NewRoute53Client(ctx context.Context) (*Route53Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to load AWS SDK config: %w", err)
	}

	return &Route53Client{
		client: route53.NewFromConfig(cfg),
	}, nil
}

// FindHostedZoneByDomain finds a Route53 hosted zone that matches the given domain
// It looks for the longest matching zone (e.g., if domain is "cluster.example.com",
// it will prefer "example.com" zone over "com" zone)
func (r *Route53Client) FindHostedZoneByDomain(ctx context.Context, domain string) (*types.HostedZone, error) {
	// List all hosted zones
	var allZones []types.HostedZone
	paginator := route53.NewListHostedZonesPaginator(r.client, &route53.ListHostedZonesInput{})

	for paginator.HasMorePages() {
		output, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list hosted zones: %w", err)
		}
		allZones = append(allZones, output.HostedZones...)
	}

	// Find the best matching zone (longest match)
	var bestMatch *types.HostedZone
	var longestMatchLen int

	for i := range allZones {
		zone := &allZones[i]
		zoneName := strings.TrimSuffix(aws.ToString(zone.Name), ".")

		// Check if domain ends with the zone name
		if strings.HasSuffix(domain, zoneName) || domain == zoneName {
			if len(zoneName) > longestMatchLen {
				longestMatchLen = len(zoneName)
				bestMatch = zone
			}
		}
	}

	if bestMatch == nil {
		return nil, fmt.Errorf("no hosted zone found matching domain: %s", domain)
	}

	return bestMatch, nil
}

// UpsertARecord creates or updates an A record (ALIAS) pointing to an AWS load balancer
func (r *Route53Client) UpsertARecord(ctx context.Context, hostedZoneID, recordName, loadBalancerDNS string) error {
	// Extract the hosted zone ID for the load balancer
	// AWS load balancers have canonical hosted zone IDs based on region
	lbHostedZoneID, err := r.getLoadBalancerHostedZoneID(loadBalancerDNS)
	if err != nil {
		return fmt.Errorf("failed to determine load balancer hosted zone ID: %w", err)
	}

	// Ensure recordName ends with a dot
	if !strings.HasSuffix(recordName, ".") {
		recordName = recordName + "."
	}

	// Ensure loadBalancerDNS ends with a dot
	if !strings.HasSuffix(loadBalancerDNS, ".") {
		loadBalancerDNS = loadBalancerDNS + "."
	}

	input := &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(hostedZoneID),
		ChangeBatch: &types.ChangeBatch{
			Changes: []types.Change{
				{
					Action: types.ChangeActionUpsert,
					ResourceRecordSet: &types.ResourceRecordSet{
						Name: aws.String(recordName),
						Type: types.RRTypeA,
						AliasTarget: &types.AliasTarget{
							HostedZoneId:         aws.String(lbHostedZoneID),
							DNSName:              aws.String(loadBalancerDNS),
							EvaluateTargetHealth: false,
						},
					},
				},
			},
		},
	}

	_, err = r.client.ChangeResourceRecordSets(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to upsert A record %s: %w", recordName, err)
	}

	return nil
}

// UpsertMultipleARecords creates or updates multiple A records (not ALIAS) with different IPs
func (r *Route53Client) UpsertMultipleARecords(ctx context.Context, hostedZoneID, recordName string, ips []string) error {
	// Ensure recordName ends with a dot
	if !strings.HasSuffix(recordName, ".") {
		recordName = recordName + "."
	}

	// Create ResourceRecords from IPs
	var resourceRecords []types.ResourceRecord
	for _, ip := range ips {
		resourceRecords = append(resourceRecords, types.ResourceRecord{
			Value: aws.String(ip),
		})
	}

	input := &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(hostedZoneID),
		ChangeBatch: &types.ChangeBatch{
			Changes: []types.Change{
				{
					Action: types.ChangeActionUpsert,
					ResourceRecordSet: &types.ResourceRecordSet{
						Name:            aws.String(recordName),
						Type:            types.RRTypeA,
						TTL:             aws.Int64(300), // 5 minutes
						ResourceRecords: resourceRecords,
					},
				},
			},
		},
	}

	_, err := r.client.ChangeResourceRecordSets(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to upsert multiple A records %s: %w", recordName, err)
	}

	return nil
}

// DeleteARecord deletes an A record from Route53
func (r *Route53Client) DeleteARecord(ctx context.Context, hostedZoneID, recordName string) error {
	// Ensure recordName ends with a dot
	if !strings.HasSuffix(recordName, ".") {
		recordName = recordName + "."
	}

	// First, get the current record to find its details
	listInput := &route53.ListResourceRecordSetsInput{
		HostedZoneId:    aws.String(hostedZoneID),
		StartRecordName: aws.String(recordName),
		StartRecordType: types.RRTypeA,
		MaxItems:        aws.Int32(1),
	}

	listOutput, err := r.client.ListResourceRecordSets(ctx, listInput)
	if err != nil {
		return fmt.Errorf("failed to list record sets for %s: %w", recordName, err)
	}

	// Check if the record exists
	var recordSet *types.ResourceRecordSet
	for i := range listOutput.ResourceRecordSets {
		rs := &listOutput.ResourceRecordSets[i]
		if aws.ToString(rs.Name) == recordName && rs.Type == types.RRTypeA {
			recordSet = rs
			break
		}
	}

	if recordSet == nil {
		// Record doesn't exist, nothing to delete
		return nil
	}

	// Delete the record
	input := &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(hostedZoneID),
		ChangeBatch: &types.ChangeBatch{
			Changes: []types.Change{
				{
					Action:            types.ChangeActionDelete,
					ResourceRecordSet: recordSet,
				},
			},
		},
	}

	_, err = r.client.ChangeResourceRecordSets(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to delete A record %s: %w", recordName, err)
	}

	return nil
}

// getLoadBalancerHostedZoneID returns the canonical hosted zone ID for an AWS load balancer
// based on the load balancer DNS name
func (r *Route53Client) getLoadBalancerHostedZoneID(lbDNS string) (string, error) {
	// Map of regions to their ELB/ALB/NLB hosted zone IDs
	// Reference: https://docs.aws.amazon.com/general/latest/gr/elb.html
	regionToZoneID := map[string]string{
		"us-east-1":      "Z35SXDOTRQ7X7K",
		"us-east-2":      "Z3AADJGX6KTTL2",
		"us-west-1":      "Z368ELLRRE2KJ0",
		"us-west-2":      "Z1H1FL5HABSF5",
		"ca-central-1":   "ZQSVJUPU6J1EY",
		"eu-central-1":   "Z215JYRZR1TBD5",
		"eu-west-1":      "Z32O12XQLNTSW2",
		"eu-west-2":      "ZHURV8PSTC4K8",
		"eu-west-3":      "Z3Q77PNBQS71R4",
		"eu-north-1":     "Z23TAZ6LKFMNIO",
		"ap-northeast-1": "Z14GRHDCWA56QT",
		"ap-northeast-2": "ZWKZPGTI48KDX",
		"ap-southeast-1": "Z1LMS91P8CMLE5",
		"ap-southeast-2": "Z1GM3OXH4ZPM65",
		"ap-south-1":     "ZP97RAFLXTNZK",
		"sa-east-1":      "Z2P70J7HTTTPLU",
	}

	// Extract region from load balancer DNS
	// Format: <name>-<id>.<region>.elb.amazonaws.com
	parts := strings.Split(lbDNS, ".")
	if len(parts) < 4 {
		return "", fmt.Errorf("invalid load balancer DNS format: %s", lbDNS)
	}

	region := parts[1]
	zoneID, ok := regionToZoneID[region]
	if !ok {
		return "", fmt.Errorf("unknown region in load balancer DNS: %s", region)
	}

	return zoneID, nil
}
