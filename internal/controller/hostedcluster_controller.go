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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	awsclient "github.com/jpacker/route53-aws-agent-hcp/internal/aws"
)

const (
	// FinalizerName is the finalizer added to HostedCluster resources
	FinalizerName = "route53.hypershift.openshift.io/dns-records"

	// LabelDNSManaged marks that DNS records are managed by this controller
	// Value "true" means DNS has been configured and reconciliation should be skipped
	LabelDNSManaged = "route53.hypershift.openshift.io/dns-managed"

	// ConditionTypeRoute53Ready indicates whether Route53 records have been created
	ConditionTypeRoute53Ready = "Route53Ready"

	// EventReasonRoute53Success indicates successful Route53 operation
	EventReasonRoute53Success = "Route53RecordsCreated"

	// EventReasonRoute53Error indicates failed Route53 operation
	EventReasonRoute53Error = "Route53Error"

	// EventReasonZoneNotFound indicates hosted zone not found
	EventReasonZoneNotFound = "HostedZoneNotFound"

	// EventReasonAWSCredentialsError indicates AWS credentials are missing or invalid
	EventReasonAWSCredentialsError = "AWSCredentialsError"

	// EventReasonAWSAuthenticationError indicates AWS authentication failed
	EventReasonAWSAuthenticationError = "AWSAuthenticationError"

	// EventReasonAWSAccessDenied indicates AWS IAM permissions are insufficient
	EventReasonAWSAccessDenied = "AWSAccessDenied"

	// EventReasonInvalidConfig indicates invalid configuration
	EventReasonInvalidConfig = "InvalidConfiguration"

	// EventReasonRoute53Cleanup indicates successful cleanup of Route53 records
	EventReasonRoute53Cleanup = "Route53RecordsDeleted"
)

// HostedClusterReconciler reconciles a HostedCluster object
type HostedClusterReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	EventRecorder record.EventRecorder
	Route53Client *awsclient.Route53Client
}

// +kubebuilder:rbac:groups=hypershift.openshift.io,resources=hostedclusters,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hypershift.openshift.io,resources=hostedclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hypershift.openshift.io,resources=hostedclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile implements the reconciliation loop for HostedCluster resources
func (r *HostedClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the HostedCluster instance
	var hostedCluster hypershiftv1beta1.HostedCluster
	if err := r.Get(ctx, req.NamespacedName, &hostedCluster); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("HostedCluster resource not found, ignoring")
			return ctrl.Result{}, nil
		}
		logger.Error(err, "Failed to get HostedCluster")
		return ctrl.Result{}, err
	}

	// Handle deletion with finalizer
	if !hostedCluster.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&hostedCluster, FinalizerName) {
			// Run finalization logic
			if err := r.finalizeHostedCluster(ctx, &hostedCluster); err != nil {
				logger.Error(err, "Failed to finalize HostedCluster")
				return ctrl.Result{}, err
			}

			// Fetch fresh copy before removing finalizer to avoid conflicts
			var freshHostedCluster hypershiftv1beta1.HostedCluster
			if err := r.Get(ctx, req.NamespacedName, &freshHostedCluster); err != nil {
				if apierrors.IsNotFound(err) {
					logger.Info("HostedCluster was deleted before finalizer removal, ignoring")
					return ctrl.Result{}, nil
				}
				logger.Error(err, "Failed to fetch fresh HostedCluster copy for finalizer removal")
				return ctrl.Result{}, err
			}

			// Remove finalizer from fresh copy
			controllerutil.RemoveFinalizer(&freshHostedCluster, FinalizerName)
			if err := r.Update(ctx, &freshHostedCluster); err != nil {
				logger.Error(err, "Failed to remove finalizer", "resourceVersion", freshHostedCluster.ResourceVersion)
				// Return error to trigger retry - the update might be conflicting with another operation
				return ctrl.Result{}, err
			}
			logger.Info("Successfully removed finalizer")
		}
		return ctrl.Result{}, nil
	}

	// Note: No need for early exit check here - the watch predicate filters out
	// resources with the dns-managed=true label, so we only reconcile when needed

	// Validate required fields
	if err := r.validateHostedCluster(&hostedCluster); err != nil {
		logger.Error(err, "Invalid HostedCluster configuration")
		r.EventRecorder.Event(&hostedCluster, corev1.EventTypeWarning, EventReasonInvalidConfig, err.Error())
		r.setCondition(&hostedCluster, ConditionTypeRoute53Ready, metav1.ConditionFalse, "InvalidConfiguration", err.Error())
		_ = r.Status().Update(ctx, &hostedCluster)
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	// Get base domain and load balancer DNS
	baseDomain := hostedCluster.Spec.DNS.BaseDomain
	loadBalancerDNS := r.getLoadBalancerDNS(&hostedCluster)

	if loadBalancerDNS == "" {
		logger.Info("Load balancer DNS not yet available, requeuing")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Find the Route53 hosted zone matching the base domain
	hostedZone, err := r.Route53Client.FindHostedZoneByDomain(ctx, baseDomain)
	if err != nil {
		logger.Error(err, "Failed to find Route53 hosted zone", "baseDomain", baseDomain)

		// Check if this is an AWS credentials/authentication error
		errMsg := err.Error()
		var reason, friendlyMsg string

		if strings.Contains(errMsg, "InvalidClientTokenId") ||
		   strings.Contains(errMsg, "security token") ||
		   strings.Contains(errMsg, "credentials") {
			reason = "AWSCredentialsError"
			friendlyMsg = fmt.Sprintf("AWS credentials error when accessing Route53: %v. Please check that AWS credentials (IAM role or access keys) are properly configured.", err)
		} else if strings.Contains(errMsg, "UnrecognizedClientException") ||
		          strings.Contains(errMsg, "InvalidSignatureException") {
			reason = "AWSAuthenticationError"
			friendlyMsg = fmt.Sprintf("AWS authentication error: %v. Please verify AWS credentials and region configuration.", err)
		} else if strings.Contains(errMsg, "AccessDenied") ||
		          strings.Contains(errMsg, "Forbidden") ||
		          strings.Contains(errMsg, "403") {
			reason = "AWSAccessDenied"
			friendlyMsg = fmt.Sprintf("AWS access denied when listing Route53 hosted zones: %v. Please verify IAM permissions include route53:ListHostedZones.", err)
		} else {
			reason = "HostedZoneNotFound"
			friendlyMsg = fmt.Sprintf("No Route53 hosted zone found matching base domain '%s': %v", baseDomain, err)
		}

		r.EventRecorder.Event(&hostedCluster, corev1.EventTypeWarning, reason, friendlyMsg)
		r.setCondition(&hostedCluster, ConditionTypeRoute53Ready, metav1.ConditionFalse, reason, friendlyMsg)
		_ = r.Status().Update(ctx, &hostedCluster)
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	hostedZoneID := strings.TrimPrefix(*hostedZone.Id, "/hostedzone/")
	logger.Info("Found Route53 hosted zone", "zoneID", hostedZoneID, "zoneName", *hostedZone.Name)

	// Add finalizer before creating records to ensure cleanup
	if !controllerutil.ContainsFinalizer(&hostedCluster, FinalizerName) {
		logger.Info("Adding finalizer to HostedCluster")
		controllerutil.AddFinalizer(&hostedCluster, FinalizerName)
		if err := r.Update(ctx, &hostedCluster); err != nil {
			logger.Error(err, "Failed to add finalizer")
			return ctrl.Result{}, err
		}
		// Requeue to continue with record creation
		return ctrl.Result{Requeue: true}, nil
	}

	// Create DNS records: api.<cluster-name>.<base-domain> and api-int.<cluster-name>.<base-domain>
	clusterName := hostedCluster.Name
	apiRecordName := fmt.Sprintf("api.%s.%s", clusterName, baseDomain)
	apiIntRecordName := fmt.Sprintf("api-int.%s.%s", clusterName, baseDomain)

	// Create api record
	logger.Info("Creating/updating Route53 A record", "record", apiRecordName, "target", loadBalancerDNS)
	if err := r.Route53Client.UpsertARecord(ctx, hostedZoneID, apiRecordName, loadBalancerDNS); err != nil {
		logger.Error(err, "Failed to create api A record")
		errMsg := fmt.Sprintf("Failed to create Route53 A record for %s: %v", apiRecordName, err)
		r.EventRecorder.Event(&hostedCluster, corev1.EventTypeWarning, EventReasonRoute53Error, errMsg)
		r.setCondition(&hostedCluster, ConditionTypeRoute53Ready, metav1.ConditionFalse, "Route53Error", errMsg)
		_ = r.Status().Update(ctx, &hostedCluster)
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	// Create api-int record
	logger.Info("Creating/updating Route53 A record", "record", apiIntRecordName, "target", loadBalancerDNS)
	if err := r.Route53Client.UpsertARecord(ctx, hostedZoneID, apiIntRecordName, loadBalancerDNS); err != nil {
		logger.Error(err, "Failed to create api-int A record")
		errMsg := fmt.Sprintf("Failed to create Route53 A record for %s: %v", apiIntRecordName, err)
		r.EventRecorder.Event(&hostedCluster, corev1.EventTypeWarning, EventReasonRoute53Error, errMsg)
		r.setCondition(&hostedCluster, ConditionTypeRoute53Ready, metav1.ConditionFalse, "Route53Error", errMsg)
		_ = r.Status().Update(ctx, &hostedCluster)
		return ctrl.Result{RequeueAfter: 1 * time.Minute}, err
	}

	// Update status to reflect successful creation
	logger.Info("Successfully created/updated Route53 records", "api", apiRecordName, "api-int", apiIntRecordName)
	successMsg := fmt.Sprintf("Created Route53 A records: %s and %s pointing to %s", apiRecordName, apiIntRecordName, loadBalancerDNS)
	r.EventRecorder.Event(&hostedCluster, corev1.EventTypeNormal, EventReasonRoute53Success, successMsg)
	r.setCondition(&hostedCluster, ConditionTypeRoute53Ready, metav1.ConditionTrue, "RecordsCreated", successMsg)

	if err := r.Status().Update(ctx, &hostedCluster); err != nil {
		logger.Error(err, "Failed to update HostedCluster status")
		return ctrl.Result{}, err
	}

	// Add label to indicate DNS is managed (watch predicate will filter future reconciliations)
	if hostedCluster.Labels == nil {
		hostedCluster.Labels = make(map[string]string)
	}
	hostedCluster.Labels[LabelDNSManaged] = "true"
	if err := r.Update(ctx, &hostedCluster); err != nil {
		logger.Error(err, "Failed to add DNS managed label")
		return ctrl.Result{}, err
	}

	logger.Info("Added DNS managed label, watch predicate will filter future reconciliations")
	return ctrl.Result{}, nil
}

// finalizeHostedCluster handles cleanup when a HostedCluster is deleted
func (r *HostedClusterReconciler) finalizeHostedCluster(ctx context.Context, hc *hypershiftv1beta1.HostedCluster) error {
	logger := log.FromContext(ctx)
	logger.Info("Finalizing HostedCluster, deleting Route53 records")

	baseDomain := hc.Spec.DNS.BaseDomain
	if baseDomain == "" {
		logger.Info("No base domain configured, skipping Route53 cleanup")
		return nil
	}

	// Find the Route53 hosted zone
	hostedZone, err := r.Route53Client.FindHostedZoneByDomain(ctx, baseDomain)
	if err != nil {
		logger.Error(err, "Failed to find Route53 hosted zone during cleanup, skipping", "baseDomain", baseDomain)
		// Don't fail finalization if we can't find the zone - it may have been deleted already
		return nil
	}

	hostedZoneID := strings.TrimPrefix(*hostedZone.Id, "/hostedzone/")
	clusterName := hc.Name
	apiRecordName := fmt.Sprintf("api.%s.%s", clusterName, baseDomain)
	apiIntRecordName := fmt.Sprintf("api-int.%s.%s", clusterName, baseDomain)

	// Delete api record
	logger.Info("Deleting Route53 A record", "record", apiRecordName)
	if err := r.Route53Client.DeleteARecord(ctx, hostedZoneID, apiRecordName); err != nil {
		logger.Error(err, "Failed to delete api A record", "record", apiRecordName)
		// Log but don't fail - record may already be deleted
	}

	// Delete api-int record
	logger.Info("Deleting Route53 A record", "record", apiIntRecordName)
	if err := r.Route53Client.DeleteARecord(ctx, hostedZoneID, apiIntRecordName); err != nil {
		logger.Error(err, "Failed to delete api-int A record", "record", apiIntRecordName)
		// Log but don't fail - record may already be deleted
	}

	logger.Info("Successfully deleted Route53 records", "api", apiRecordName, "api-int", apiIntRecordName)
	r.EventRecorder.Event(hc, corev1.EventTypeNormal, EventReasonRoute53Cleanup,
		fmt.Sprintf("Deleted Route53 A records: %s and %s", apiRecordName, apiIntRecordName))

	return nil
}

// validateHostedCluster validates that the HostedCluster has required fields
func (r *HostedClusterReconciler) validateHostedCluster(hc *hypershiftv1beta1.HostedCluster) error {
	if hc.Spec.DNS.BaseDomain == "" {
		return fmt.Errorf("spec.dns.baseDomain is required")
	}
	return nil
}

// getLoadBalancerDNS extracts the load balancer DNS from the HostedCluster status
func (r *HostedClusterReconciler) getLoadBalancerDNS(hc *hypershiftv1beta1.HostedCluster) string {
	// The load balancer DNS is typically in the status
	// This may vary based on the actual Hypershift API structure
	// Common locations:
	// - status.platform.aws.loadBalancerDNS
	// - status.controlPlaneEndpoint.host

	// Check if the control plane endpoint is available
	if hc.Status.ControlPlaneEndpoint.Host != "" {
		return hc.Status.ControlPlaneEndpoint.Host
	}

	// Alternative: check platform-specific status
	// You may need to adjust this based on actual API structure
	return ""
}

// setCondition sets a condition on the HostedCluster status
func (r *HostedClusterReconciler) setCondition(hc *hypershiftv1beta1.HostedCluster, conditionType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             status,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: hc.Generation,
	}

	// Find and update existing condition or append new one
	found := false
	for i, existing := range hc.Status.Conditions {
		if existing.Type == conditionType {
			// Only update if status changed
			if existing.Status != status {
				hc.Status.Conditions[i] = condition
			} else {
				// Keep the original LastTransitionTime if status hasn't changed
				condition.LastTransitionTime = existing.LastTransitionTime
				hc.Status.Conditions[i] = condition
			}
			found = true
			break
		}
	}

	if !found {
		hc.Status.Conditions = append(hc.Status.Conditions, condition)
	}
}

// SetupWithManager sets up the controller with the Manager
func (r *HostedClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&hypershiftv1beta1.HostedCluster{}).
		WithEventFilter(predicate.Funcs{
			// CreateFunc: Allow all create events (new HostedClusters need DNS setup)
			CreateFunc: func(e event.CreateEvent) bool {
				return true
			},
			// UpdateFunc: Filter based on label and deletion
			UpdateFunc: func(e event.UpdateEvent) bool {
				hc, ok := e.ObjectNew.(*hypershiftv1beta1.HostedCluster)
				if !ok {
					return true // Allow if type assertion fails
				}

				// Always reconcile if being deleted (finalizer cleanup)
				if !hc.DeletionTimestamp.IsZero() {
					return true
				}

				// Skip reconciliation if DNS is already managed
				if hc.Labels != nil {
					if managed, exists := hc.Labels[LabelDNSManaged]; exists && managed == "true" {
						return false
					}
				}

				return true
			},
			// DeleteFunc: No need to reconcile on delete (finalizer handles cleanup)
			DeleteFunc: func(e event.DeleteEvent) bool {
				return false
			},
			// GenericFunc: Allow generic events
			GenericFunc: func(e event.GenericEvent) bool {
				return true
			},
		}).
		Complete(r)
}
