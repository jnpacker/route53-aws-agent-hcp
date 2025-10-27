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

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	awsclient "github.com/jpacker/route53-aws-agent-hcp/internal/aws"
)

const (
	// NodePoolFinalizerName is the finalizer for NodePool resources
	NodePoolFinalizerName = "route53.hypershift.openshift.io/nodepool-dns"

	// LabelNodePoolDNSManaged marks that NodePool DNS has been configured
	LabelNodePoolDNSManaged = "route53.hypershift.openshift.io/nodepool-dns-managed"

	// EventReasonNodePoolDNSSuccess indicates successful NodePool DNS operation
	EventReasonNodePoolDNSSuccess = "NodePoolDNSRecordsCreated"

	// EventReasonNodePoolDNSError indicates failed NodePool DNS operation
	EventReasonNodePoolDNSError = "NodePoolDNSError"

	// EventReasonNodePoolDNSCleanup indicates successful cleanup
	EventReasonNodePoolDNSCleanup = "NodePoolDNSRecordsDeleted"
)

// NodePoolReconciler reconciles NodePool resources to manage *.apps DNS records
type NodePoolReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	EventRecorder record.EventRecorder
	Route53Client *awsclient.Route53Client
}

// +kubebuilder:rbac:groups=hypershift.openshift.io,resources=nodepools,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hypershift.openshift.io,resources=nodepools/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hypershift.openshift.io,resources=nodepools/finalizers,verbs=update
// +kubebuilder:rbac:groups=agent-install.openshift.io,resources=agents,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile manages *.apps.<cluster>.<base-domain> DNS records
func (r *NodePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the NodePool instance
	var nodePool hypershiftv1beta1.NodePool
	if err := r.Get(ctx, req.NamespacedName, &nodePool); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("NodePool resource not found, ignoring")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get NodePool")
		return ctrl.Result{}, err
	}

	// Handle deletion with finalizer
	if !nodePool.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&nodePool, NodePoolFinalizerName) {
			// Run finalization logic
			if err := r.finalizeNodePool(ctx, &nodePool); err != nil {
				logger.Error(err, "Failed to finalize NodePool")
				return ctrl.Result{}, err
			}

			// Fetch fresh copy before removing finalizer to avoid conflicts
			var freshNodePool hypershiftv1beta1.NodePool
			if err := r.Get(ctx, req.NamespacedName, &freshNodePool); err != nil {
				if apierrors.IsNotFound(err) {
					logger.Info("NodePool was deleted before finalizer removal, ignoring")
					return ctrl.Result{}, nil
				}
				logger.Error(err, "Failed to fetch fresh NodePool copy for finalizer removal")
				return ctrl.Result{}, err
			}

			// Remove finalizer from fresh copy
			controllerutil.RemoveFinalizer(&freshNodePool, NodePoolFinalizerName)
			if err := r.Update(ctx, &freshNodePool); err != nil {
				logger.Error(err, "Failed to remove finalizer", "resourceVersion", freshNodePool.ResourceVersion)
				return ctrl.Result{}, err
			}
			logger.Info("Successfully removed finalizer")
		}
		return ctrl.Result{}, nil
	}

	// Get the associated HostedCluster
	hostedCluster, err := r.getHostedClusterForNodePool(ctx, &nodePool)
	if err != nil {
		logger.Error(err, "Failed to get HostedCluster for NodePool")
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	baseDomain := hostedCluster.Spec.DNS.BaseDomain
	clusterName := hostedCluster.Name

	// Get Agent IPs for this NodePool
	agentIPs, err := r.getAgentIPsForNodePool(ctx, &nodePool)
	if err != nil {
		logger.Error(err, "Failed to get Agent IPs")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Find the Route53 hosted zone (needed whether we're creating or deleting)
	hostedZone, err := r.Route53Client.FindHostedZoneByDomain(ctx, baseDomain)
	if err != nil {
		logger.Error(err, "Failed to find Route53 hosted zone", "baseDomain", baseDomain)
		errMsg := fmt.Sprintf("No Route53 hosted zone found for *.apps record: %v", err)
		r.EventRecorder.Event(&nodePool, corev1.EventTypeWarning, EventReasonNodePoolDNSError, errMsg)
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	hostedZoneID := strings.TrimPrefix(*hostedZone.Id, "/hostedzone/")
	appsRecordName := fmt.Sprintf("*.apps.%s.%s", clusterName, baseDomain)

	if len(agentIPs) == 0 {
		// No agents available - delete the DNS record if it exists
		logger.Info("No Agent IPs available, deleting *.apps DNS record", "record", appsRecordName)
		if err := r.Route53Client.DeleteARecord(ctx, hostedZoneID, appsRecordName); err != nil {
			logger.Error(err, "Failed to delete *.apps A record during cleanup", "record", appsRecordName)
			// Still return error so we retry - don't silently fail
			return ctrl.Result{RequeueAfter: 30 * time.Second}, err
		}
		logger.Info("Successfully deleted *.apps DNS record", "record", appsRecordName)
		r.EventRecorder.Event(&nodePool, corev1.EventTypeNormal, EventReasonNodePoolDNSCleanup, 
			fmt.Sprintf("Deleted Route53 *.apps record: %s", appsRecordName))
		return ctrl.Result{}, nil
	}

	logger.Info("Found Agent IPs", "count", len(agentIPs), "ips", agentIPs)

	// Add finalizer before creating records
	if !controllerutil.ContainsFinalizer(&nodePool, NodePoolFinalizerName) {
		logger.Info("Adding finalizer to NodePool")
		controllerutil.AddFinalizer(&nodePool, NodePoolFinalizerName)
		if err := r.Update(ctx, &nodePool); err != nil {
			logger.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Create *.apps DNS record with multiple A records
	logger.Info("Creating/updating Route53 *.apps A records", "record", appsRecordName, "ips", agentIPs)

	if err := r.Route53Client.UpsertMultipleARecords(ctx, hostedZoneID, appsRecordName, agentIPs); err != nil {
		logger.Error(err, "Failed to create *.apps A records")
		errMsg := fmt.Sprintf("Failed to create Route53 *.apps record: %v", err)
		r.EventRecorder.Event(&nodePool, corev1.EventTypeWarning, EventReasonNodePoolDNSError, errMsg)
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	// Success - add completion label
	logger.Info("Successfully created/updated *.apps DNS records")
	successMsg := fmt.Sprintf("Created Route53 *.apps record: %s with %d IPs", appsRecordName, len(agentIPs))
	r.EventRecorder.Event(&nodePool, corev1.EventTypeNormal, EventReasonNodePoolDNSSuccess, successMsg)

	// Add label to prevent future reconciliation
	if nodePool.Labels == nil {
		nodePool.Labels = make(map[string]string)
	}
	nodePool.Labels[LabelNodePoolDNSManaged] = "true"
	if err := r.Update(ctx, &nodePool); err != nil {
		logger.Error(err, "Failed to add DNS managed label")
		return ctrl.Result{}, err
	}

	logger.Info("Added DNS managed label, watch predicate will filter future reconciliations")
	return ctrl.Result{}, nil
}

// getHostedClusterForNodePool finds the HostedCluster that owns this NodePool
func (r *NodePoolReconciler) getHostedClusterForNodePool(ctx context.Context, nodePool *hypershiftv1beta1.NodePool) (*hypershiftv1beta1.HostedCluster, error) {
	// NodePool spec contains the cluster name reference
	clusterName := nodePool.Spec.ClusterName

	var hostedCluster hypershiftv1beta1.HostedCluster
	// HostedCluster is in the same namespace as NodePool
	if err := r.Get(ctx, types.NamespacedName{
		Name:      clusterName,
		Namespace: nodePool.Namespace,
	}, &hostedCluster); err != nil {
		return nil, fmt.Errorf("failed to get HostedCluster %s: %w", clusterName, err)
	}

	return &hostedCluster, nil
}

// getAgentIPsForNodePool retrieves the IPs of all Agents assigned to this NodePool
func (r *NodePoolReconciler) getAgentIPsForNodePool(ctx context.Context, nodePool *hypershiftv1beta1.NodePool) ([]string, error) {
	logger := log.FromContext(ctx)

	// Agents are from the agent-install.openshift.io API group
	// The Agent CRD structure (from agent-install.openshift.io/v1beta1):
	// - spec.clusterDeploymentName.name: references the cluster
	// - status.inventory.interfaces[].ipV4Addresses[]: contains the IPs (note: capital V)
	//
	// Use unstructured client to list Agents without importing the CRD
	// Agents can be in any namespace, so we search cluster-wide

	clusterName := nodePool.Spec.ClusterName

	logger.Info("Looking for Agents cluster-wide", "cluster", clusterName)

	// Define the Agent GVK (GroupVersionKind)
	agentGVK := schema.GroupVersionKind{
		Group:   "agent-install.openshift.io",
		Version: "v1beta1",
		Kind:    "Agent",
	}

	// List all Agents across all namespaces
	agentList := &unstructured.UnstructuredList{}
	agentList.SetGroupVersionKind(agentGVK)

	if err := r.List(ctx, agentList); err != nil {
		return nil, fmt.Errorf("failed to list Agents: %w", err)
	}

	var ips []string
	ipSet := make(map[string]bool) // For deduplication

	for _, agent := range agentList.Items {
		// Filter by cluster deployment name
		clusterDeploymentName, found, err := unstructured.NestedString(agent.Object, "spec", "clusterDeploymentName", "name")
		if err != nil || !found || clusterDeploymentName != clusterName {
			continue
		}

		logger.Info("Found Agent for cluster", "agent", agent.GetName(), "cluster", clusterName)

		// Extract IPs from status.inventory.interfaces[].ipv4Addresses[]
		interfaces, found, err := unstructured.NestedSlice(agent.Object, "status", "inventory", "interfaces")
		if err != nil || !found {
			logger.V(1).Info("No inventory interfaces found for Agent", "agent", agent.GetName())
			continue
		}

		for _, iface := range interfaces {
			ifaceMap, ok := iface.(map[string]interface{})
			if !ok {
				continue
			}

			// Note: Field name is ipV4Addresses (capital V)
			ipv4Addresses, found, err := unstructured.NestedStringSlice(ifaceMap, "ipV4Addresses")
			if err != nil || !found {
				continue
			}

			for _, ipv4 := range ipv4Addresses {
				// Remove CIDR notation if present (e.g., "192.168.1.10/24" -> "192.168.1.10")
				ip := strings.Split(ipv4, "/")[0]

				// Skip localhost and link-local addresses
				if strings.HasPrefix(ip, "127.") || strings.HasPrefix(ip, "169.254.") {
					continue
				}

				if !ipSet[ip] {
					ips = append(ips, ip)
					ipSet[ip] = true
				}
			}
		}
	}

	logger.Info("Discovered Agent IPs", "count", len(ips), "ips", ips)
	return ips, nil
}

// finalizeNodePool handles cleanup when a NodePool is deleted
func (r *NodePoolReconciler) finalizeNodePool(ctx context.Context, nodePool *hypershiftv1beta1.NodePool) error {
	logger := log.FromContext(ctx)
	logger.Info("Finalizing NodePool, deleting *.apps DNS records")

	// Get the associated HostedCluster
	hostedCluster, err := r.getHostedClusterForNodePool(ctx, nodePool)
	if err != nil {
		logger.Error(err, "Failed to get HostedCluster during cleanup, skipping")
		return nil
	}

	baseDomain := hostedCluster.Spec.DNS.BaseDomain
	clusterName := hostedCluster.Name

	// Find the Route53 hosted zone
	hostedZone, err := r.Route53Client.FindHostedZoneByDomain(ctx, baseDomain)
	if err != nil {
		logger.Error(err, "Failed to find Route53 hosted zone during cleanup, skipping")
		return nil
	}

	hostedZoneID := strings.TrimPrefix(*hostedZone.Id, "/hostedzone/")
	appsRecordName := fmt.Sprintf("*.apps.%s.%s", clusterName, baseDomain)

	// Delete *.apps record
	logger.Info("Deleting Route53 *.apps A record", "record", appsRecordName)
	if err := r.Route53Client.DeleteARecord(ctx, hostedZoneID, appsRecordName); err != nil {
		logger.Error(err, "Failed to delete *.apps A record", "record", appsRecordName)
	}

	logger.Info("Successfully deleted *.apps DNS record", "record", appsRecordName)
	r.EventRecorder.Event(nodePool, corev1.EventTypeNormal, EventReasonNodePoolDNSCleanup,
		fmt.Sprintf("Deleted Route53 *.apps record: %s", appsRecordName))

	return nil
}

// SetupWithManager sets up the controller with the Manager
func (r *NodePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Define Agent as unstructured for watching
	agentGVK := schema.GroupVersionKind{
		Group:   "agent-install.openshift.io",
		Version: "v1beta1",
		Kind:    "Agent",
	}

	agentUnstructured := &unstructured.Unstructured{}
	agentUnstructured.SetGroupVersionKind(agentGVK)

	return ctrl.NewControllerManagedBy(mgr).
		For(&hypershiftv1beta1.NodePool{}).
		// Watch Agents to trigger reconciliation when Agent IPs change or Agents are added/removed
		Watches(
			agentUnstructured,
			handler.EnqueueRequestsFromMapFunc(r.findNodePoolsForAgent),
		).
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				np, ok := e.ObjectNew.(*hypershiftv1beta1.NodePool)
				if !ok {
					return true
				}

				// Always reconcile if being deleted
				if !np.DeletionTimestamp.IsZero() {
					return true
				}

				// Skip if already managed
				if np.Labels != nil {
					if managed, exists := np.Labels[LabelNodePoolDNSManaged]; exists && managed == "true" {
						return false
					}
				}

				return true
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool {
				return true
			},
		}).
		Complete(r)
}

// findNodePoolsForAgent maps Agent changes to NodePool reconciliations
// When an Agent is added/removed/updated, this finds all NodePools for that cluster
func (r *NodePoolReconciler) findNodePoolsForAgent(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx)
	agent, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}

	// Get cluster deployment name from Agent
	clusterDeploymentName, found, err := unstructured.NestedString(agent.Object, "spec", "clusterDeploymentName", "name")
	if err != nil || !found {
		logger.V(1).Info("Agent has no clusterDeploymentName, skipping", "agent", agent.GetName())
		return nil
	}

	logger.Info("Agent changed, finding NodePools for cluster", "agent", agent.GetName(), "cluster", clusterDeploymentName)

	// Find all NodePools for this cluster across all namespaces
	var nodePoolList hypershiftv1beta1.NodePoolList
	if err := r.List(ctx, &nodePoolList); err != nil {
		logger.Error(err, "Failed to list NodePools for Agent change")
		return nil
	}

	var requests []reconcile.Request
	for _, np := range nodePoolList.Items {
		if np.Spec.ClusterName == clusterDeploymentName {
			logger.Info("Triggering NodePool reconciliation due to Agent change",
				"nodepool", np.Name,
				"namespace", np.Namespace,
				"cluster", clusterDeploymentName,
				"agent", agent.GetName())

			// Remove the DNS managed label to force reconciliation
			if np.Labels != nil {
				if _, exists := np.Labels[LabelNodePoolDNSManaged]; exists {
					delete(np.Labels, LabelNodePoolDNSManaged)
					if err := r.Update(ctx, &np); err != nil {
						logger.Error(err, "Failed to remove DNS managed label", "nodepool", np.Name)
					}
				}
			}

			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      np.Name,
					Namespace: np.Namespace,
				},
			})
		}
	}

	if len(requests) > 0 {
		logger.Info("Enqueued NodePool reconciliations", "count", len(requests), "cluster", clusterDeploymentName)
	}

	return requests
}
