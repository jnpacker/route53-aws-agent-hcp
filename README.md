# Route53 AWS Agent for HCP (Hosted Control Planes)

A Kubernetes controller that watches for HyperShift `HostedCluster` resources and automatically manages AWS Route53 DNS records.

## Quick Start

**New to this project?** See [QUICKSTART.md](QUICKSTART.md) for a step-by-step guide with copy-paste commands.

> **OpenShift Users**: This controller works with OpenShift/ROSA/HyperShift using STS (Security Token Service). The setup is similar to EKS IRSA but uses OpenShift's OIDC provider. See [Option 1](#option-1-openshift-sts-recommended-for-openshiftrosa-hypershift) in the IAM section.

**Summary:**
1. **[Configure AWS CLI](#configuring-aws-cli)** - Set up `aws configure` for testing
2. **[Set up AWS IAM permissions](#aws-iam-permissions)** - Create IAM role/user with Route53 access (Option 1 for OpenShift)
3. **[Build the controller](#building)** - `make build` or `make docker-build`
4. **[Deploy](#deployment)** - `make deploy` and annotate ServiceAccount
5. **[Verify](#troubleshooting)** - Check logs and events

## Overview

This controller:
- **Watches for `HostedCluster` resources** and creates DNS records for cluster API endpoints
- **Watches for `NodePool` resources** and creates DNS records for application ingress
- Extracts the `baseDomain` from the HostedCluster spec
- Finds the matching Route53 hosted zone in AWS
- Creates DNS A records for:
  - `api.<cluster-name>.<base-domain>` → ALIAS to AWS Load Balancer (from HostedCluster)
  - `api-int.<cluster-name>.<base-domain>` → ALIAS to AWS Load Balancer (from HostedCluster)
  - `*.apps.<cluster-name>.<base-domain>` → Multiple A records pointing to Agent IPs (from NodePool)
- Discovers Agent IPs from NodePool-associated Agents using unstructured client queries
- Logs errors and creates Kubernetes events when zones cannot be found
- Updates resources with conditions and labels indicating DNS readiness
- Automatically cleans up DNS records when resources are deleted using finalizers

## Prerequisites

- Go 1.22+
- Access to a Kubernetes cluster with HyperShift installed
- AWS credentials with Route53 permissions
- Docker (for building container images)

## AWS IAM Permissions

The controller requires specific AWS IAM permissions to manage Route53 DNS records.

### Required IAM Policy

Create an IAM policy with the following permissions:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "Route53ListHostedZones",
      "Effect": "Allow",
      "Action": [
        "route53:ListHostedZones"
      ],
      "Resource": "*"
    },
    {
      "Sid": "Route53ManageDNSRecords",
      "Effect": "Allow",
      "Action": [
        "route53:GetHostedZone",
        "route53:ListResourceRecordSets",
        "route53:ChangeResourceRecordSets"
      ],
      "Resource": "arn:aws:route53:::hostedzone/*"
    }
  ]
}
```

**Note**: `route53:ListHostedZones` requires `Resource: "*"` as it's not zone-specific. For tighter security, restrict `ChangeResourceRecordSets` to specific hosted zones if known.

### Creating the IAM Role

#### Option 1: OpenShift STS (Recommended for OpenShift/ROSA/HyperShift)

For OpenShift clusters with STS (Security Token Service) enabled:

1. **Verify your cluster uses STS**:
   ```bash
   # Check if your cluster has the cloud-credential-operator
   oc get cloudcredential cluster -o yaml

   # Look for mode: Manual or mode: Mint with STS
   ```

2. **Create the IAM policy**:
   ```bash
   cat > route53-policy.json <<EOF
   {
     "Version": "2012-10-17",
     "Statement": [
       {
         "Sid": "Route53ListHostedZones",
         "Effect": "Allow",
         "Action": ["route53:ListHostedZones"],
         "Resource": "*"
       },
       {
         "Sid": "Route53ManageDNSRecords",
         "Effect": "Allow",
         "Action": [
           "route53:GetHostedZone",
           "route53:ListResourceRecordSets",
           "route53:ChangeResourceRecordSets"
         ],
         "Resource": "arn:aws:route53:::hostedzone/*"
       }
     ]
   }
   EOF

   aws iam create-policy \
     --policy-name Route53DNSManagement \
     --policy-document file://route53-policy.json
   ```

3. **Create IAM role for the controller**:
   ```bash
   # Get your cluster's OIDC provider
   AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)

   # For ROSA/HyperShift clusters, get the OIDC provider from the cluster
   OIDC_PROVIDER=$(oc get authentication.config.openshift.io cluster -o json | \
     jq -r .spec.serviceAccountIssuer | sed 's|https://||')

   # If that doesn't work, check the infrastructure object
   # OIDC_PROVIDER=$(oc get infrastructure cluster -o jsonpath='{.status.platformStatus.aws.resourceTags[?(@.key=="red-hat-managed")].value}')

   cat > trust-policy.json <<EOF
   {
     "Version": "2012-10-17",
     "Statement": [
       {
         "Effect": "Allow",
         "Principal": {
           "Federated": "arn:aws:iam::${AWS_ACCOUNT_ID}:oidc-provider/${OIDC_PROVIDER}"
         },
         "Action": "sts:AssumeRoleWithWebIdentity",
         "Condition": {
           "StringEquals": {
             "${OIDC_PROVIDER}:sub": "system:serviceaccount:hypershift:route53-controller"
           }
         }
       }
     ]
   }
   EOF

   aws iam create-role \
     --role-name Route53ControllerRole \
     --assume-role-policy-document file://trust-policy.json

   aws iam attach-role-policy \
     --role-name Route53ControllerRole \
     --policy-arn arn:aws:iam::${AWS_ACCOUNT_ID}:policy/Route53DNSManagement
   ```

4. **Annotate the ServiceAccount**:
   ```bash
   oc annotate serviceaccount route53-controller \
     -n hypershift \
     eks.amazonaws.com/role-arn=arn:aws:iam::${AWS_ACCOUNT_ID}:role/Route53ControllerRole
   ```

   **Note**: The annotation uses `eks.amazonaws.com/role-arn` even for OpenShift - this is the standard annotation that the AWS SDK recognizes.

#### Option 1b: IAM Role for Service Accounts (IRSA) - For EKS Clusters Only

1. **Create the IAM policy**:
   ```bash
   cat > route53-policy.json <<EOF
   {
     "Version": "2012-10-17",
     "Statement": [
       {
         "Sid": "Route53ListHostedZones",
         "Effect": "Allow",
         "Action": ["route53:ListHostedZones"],
         "Resource": "*"
       },
       {
         "Sid": "Route53ManageDNSRecords",
         "Effect": "Allow",
         "Action": [
           "route53:GetHostedZone",
           "route53:ListResourceRecordSets",
           "route53:ChangeResourceRecordSets"
         ],
         "Resource": "arn:aws:route53:::hostedzone/*"
       }
     ]
   }
   EOF

   aws iam create-policy \
     --policy-name Route53DNSManagement \
     --policy-document file://route53-policy.json
   ```

2. **Create IAM role with trust relationship for your EKS cluster**:
   ```bash
   # Set variables
   CLUSTER_NAME="your-eks-cluster"
   AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
   OIDC_PROVIDER=$(aws eks describe-cluster --name $CLUSTER_NAME --query "cluster.identity.oidc.issuer" --output text | sed -e "s/^https:\/\///")
   NAMESPACE="hypershift"
   SERVICE_ACCOUNT="route53-controller"

   # Create trust policy
   cat > trust-policy.json <<EOF
   {
     "Version": "2012-10-17",
     "Statement": [
       {
         "Effect": "Allow",
         "Principal": {
           "Federated": "arn:aws:iam::${AWS_ACCOUNT_ID}:oidc-provider/${OIDC_PROVIDER}"
         },
         "Action": "sts:AssumeRoleWithWebIdentity",
         "Condition": {
           "StringEquals": {
             "${OIDC_PROVIDER}:sub": "system:serviceaccount:${NAMESPACE}:${SERVICE_ACCOUNT}",
             "${OIDC_PROVIDER}:aud": "sts.amazonaws.com"
           }
         }
       }
     ]
   }
   EOF

   # Create the role
   aws iam create-role \
     --role-name Route53ControllerRole \
     --assume-role-policy-document file://trust-policy.json

   # Attach the policy
   aws iam attach-role-policy \
     --role-name Route53ControllerRole \
     --policy-arn arn:aws:iam::${AWS_ACCOUNT_ID}:policy/Route53DNSManagement
   ```

3. **Annotate the ServiceAccount**:
   ```bash
   kubectl annotate serviceaccount route53-controller \
     -n hypershift \
     eks.amazonaws.com/role-arn=arn:aws:iam::${AWS_ACCOUNT_ID}:role/Route53ControllerRole
   ```

#### Option 2: IAM User with Access Keys (Not Recommended for Production)

1. **Create IAM user**:
   ```bash
   aws iam create-user --user-name route53-controller
   ```

2. **Attach policy to user**:
   ```bash
   aws iam attach-user-policy \
     --user-name route53-controller \
     --policy-arn arn:aws:iam::${AWS_ACCOUNT_ID}:policy/Route53DNSManagement
   ```

3. **Create access keys**:
   ```bash
   aws iam create-access-key --user-name route53-controller
   ```

4. **Create Kubernetes secret**:
   ```bash
   kubectl create secret generic aws-credentials \
     --from-literal=aws_access_key_id=YOUR_ACCESS_KEY \
     --from-literal=aws_secret_access_key=YOUR_SECRET_KEY \
     -n hypershift
   ```

5. **Uncomment the credential environment variables** in `deploy/deployment.yaml`

#### Option 3: EC2 Instance Profile (For self-managed Kubernetes on EC2)

1. **Create the IAM role**:
   ```bash
   # Create trust policy for EC2
   cat > ec2-trust-policy.json <<EOF
   {
     "Version": "2012-10-17",
     "Statement": [
       {
         "Effect": "Allow",
         "Principal": {
           "Service": "ec2.amazonaws.com"
         },
         "Action": "sts:AssumeRole"
       }
     ]
   }
   EOF

   aws iam create-role \
     --role-name Route53ControllerEC2Role \
     --assume-role-policy-document file://ec2-trust-policy.json

   aws iam attach-role-policy \
     --role-name Route53ControllerEC2Role \
     --policy-arn arn:aws:iam::${AWS_ACCOUNT_ID}:policy/Route53DNSManagement
   ```

2. **Create instance profile and attach to EC2 instances**:
   ```bash
   aws iam create-instance-profile --instance-profile-name Route53ControllerProfile
   aws iam add-role-to-instance-profile \
     --instance-profile-name Route53ControllerProfile \
     --role-name Route53ControllerEC2Role

   # Attach to your EC2 instances
   aws ec2 associate-iam-instance-profile \
     --instance-id i-xxxxx \
     --iam-instance-profile Name=Route53ControllerProfile
   ```

### AWS Credentials Overview

Different methods for providing AWS credentials:

| Method | AWS CLI | Controller Pod | Best For |
|--------|---------|----------------|----------|
| **OpenShift STS** | `aws configure` or env vars | ServiceAccount annotation | Production OpenShift/ROSA/HyperShift |
| **IRSA (EKS)** | `aws configure` or env vars | ServiceAccount annotation | Production EKS (not OpenShift) |
| **Static Keys** | `aws configure` or env vars | Kubernetes Secret | Development, testing |
| **EC2 Instance Profile** | Automatic (metadata) | Automatic (metadata) | Self-managed on EC2 |
| **Environment Variables** | `export AWS_*` | Pod env vars | Local testing |

**Credential Storage Locations:**
- AWS CLI: `~/.aws/credentials` and `~/.aws/config`
- Controller Pod: Injected via STS/IRSA, Secret, or EC2 metadata
- Environment Variables: Current shell session only

**Important**: OpenShift STS and EKS IRSA use similar mechanisms but have different OIDC provider configurations. Use Option 1 for OpenShift, Option 1b for EKS.

### Configuring AWS CLI

Before deploying the controller, configure your AWS CLI to test the setup:

#### Initial AWS CLI Setup

If you haven't configured AWS CLI yet:

```bash
# Install AWS CLI (if not already installed)
# For Linux:
curl "https://awscli.amazonaws.com/awscli-exe-linux-x86_64.zip" -o "awscliv2.zip"
unzip awscliv2.zip
sudo ./aws/install

# For macOS:
brew install awscli

# Configure AWS CLI with your credentials
aws configure
```

When prompted, enter:
- **AWS Access Key ID**: Your IAM user access key
- **AWS Secret Access Key**: Your IAM user secret key
- **Default region name**: e.g., `us-east-1`
- **Default output format**: `json` (recommended)

#### Using Named Profiles

For managing multiple AWS accounts or roles:

```bash
# Configure a named profile
aws configure --profile route53-controller

# Use the profile for commands
aws route53 list-hosted-zones --profile route53-controller

# Set as default for current shell session
export AWS_PROFILE=route53-controller
```

#### Using Environment Variables

Alternative to `aws configure`:

```bash
# Set AWS credentials via environment variables
export AWS_ACCESS_KEY_ID="your-access-key-id"
export AWS_SECRET_ACCESS_KEY="your-secret-access-key"
export AWS_DEFAULT_REGION="us-east-1"

# Verify
aws sts get-caller-identity
```

#### Using IAM Role (For EC2/EKS)

If running on EC2 or using IAM roles:

```bash
# No configuration needed - credentials are automatic
# Just verify the role is working
aws sts get-caller-identity

# Should show the assumed role ARN
```

### Verifying IAM Permissions

Test that the credentials work correctly:

```bash
# 1. Verify authentication
aws sts get-caller-identity
# Should show your IAM user/role information

# 2. Test Route53 list permission
aws route53 list-hosted-zones
# Should return your hosted zones without errors

# 3. Test Route53 get permission (replace ZONE_ID)
aws route53 get-hosted-zone --id /hostedzone/Z1234567890ABC

# 4. List existing records in a zone
aws route53 list-resource-record-sets --hosted-zone-id Z1234567890ABC
```

If you see errors like:
- **"Unable to locate credentials"**: AWS CLI is not configured
- **"InvalidClientTokenId"**: Access key is invalid or deactivated
- **"AccessDenied"**: IAM permissions are missing - review the [IAM policy](#required-iam-policy)

### Testing Without Deploying

You can test the IAM permissions match what the controller needs:

```bash
# Create a test script
cat > test-permissions.sh <<'EOF'
#!/bin/bash
set -e

echo "Testing Route53 permissions..."

# Test 1: List hosted zones
echo "✓ Testing route53:ListHostedZones..."
ZONES=$(aws route53 list-hosted-zones --output json)
echo "  Found $(echo $ZONES | jq '.HostedZones | length') hosted zones"

# Get first zone ID for testing
ZONE_ID=$(echo $ZONES | jq -r '.HostedZones[0].Id // empty' | sed 's|/hostedzone/||')

if [ -z "$ZONE_ID" ]; then
  echo "  ⚠ No hosted zones found. Create one for testing."
  exit 0
fi

echo "  Using zone: $ZONE_ID"

# Test 2: Get hosted zone
echo "✓ Testing route53:GetHostedZone..."
aws route53 get-hosted-zone --id $ZONE_ID > /dev/null

# Test 3: List resource record sets
echo "✓ Testing route53:ListResourceRecordSets..."
aws route53 list-resource-record-sets --hosted-zone-id $ZONE_ID > /dev/null

echo ""
echo "✅ All required Route53 permissions verified!"
echo "   You can proceed with deploying the controller."
EOF

chmod +x test-permissions.sh
./test-permissions.sh
```

## Building

```bash
# Build locally
make build

# Build Docker image
make docker-build

# Push Docker image (update image name in Makefile first)
make docker-push
```

## Deployment

**Prerequisites**: Complete the [AWS IAM Permissions](#aws-iam-permissions) section above to create the necessary IAM role or user.

### Deploy the Controller

1. **Apply RBAC and Deployment**:
   ```bash
   # For OpenShift
   oc apply -f deploy/rbac.yaml
   oc apply -f deploy/deployment.yaml

   # Or use make
   make deploy
   ```

   This will create:
   - ServiceAccount `route53-controller`
   - ClusterRole with required permissions
   - ClusterRoleBinding
   - Deployment with the controller pod

2. **For OpenShift STS - Annotate the ServiceAccount**:
   ```bash
   oc annotate serviceaccount route53-controller \
     -n hypershift \
     eks.amazonaws.com/role-arn=arn:aws:iam::YOUR_ACCOUNT_ID:role/Route53ControllerRole
   ```

   **Important**: Even though this is OpenShift, the annotation key is `eks.amazonaws.com/role-arn`. This is the standard annotation that AWS SDKs recognize for assuming IAM roles via web identity tokens.

3. **Verify deployment**:
   ```bash
   # For OpenShift
   oc get deployment -n hypershift route53-controller
   oc logs -n hypershift deployment/route53-controller

   # For Kubernetes
   kubectl get deployment -n hypershift route53-controller
   kubectl logs -n hypershift deployment/route53-controller
   ```

### Configuration Options

The deployment supports different AWS credential methods:

- **OpenShift STS**: ServiceAccount annotation set via `oc annotate` (see Option 1 in IAM Permissions)
- **IRSA (EKS)**: ServiceAccount annotation set via `kubectl annotate` (see Option 1b - EKS only)
- **Static Credentials**: Uncomment environment variables in `deploy/deployment.yaml` (see Option 2)
- **EC2 Instance Profile**: No additional configuration needed if EC2 nodes have the role attached (see Option 3)

## Configuration

The controller can be configured via command-line flags:

- `--metrics-bind-address`: The address the metric endpoint binds to (default: `:8080`)
- `--health-probe-bind-address`: The address the probe endpoint binds to (default: `:8081`)
- `--leader-elect`: Enable leader election for controller manager (default: `false`)

## How It Works

### HostedCluster Controller

Manages API endpoint DNS records (`api.*` and `api-int.*`):

1. Watches `HostedCluster` resources with an event filter predicate
2. When a HostedCluster is created or updated:
   - **Watch predicate filtering**: Resources with label `route53.hypershift.openshift.io/dns-managed=true` are filtered out at the watch level (controller never triggered)
   - Deletion events always pass through the predicate to ensure proper cleanup
   - Validates that `spec.dns.baseDomain` is set
   - Waits for the load balancer DNS to be available in `status.controlPlaneEndpoint.host`
   - Searches Route53 for a hosted zone matching the base domain (finds longest match)
   - **Adds a finalizer** to the HostedCluster before creating records
   - Creates/updates ALIAS A records for `api` and `api-int` subdomains pointing to the load balancer
   - **Adds label** `route53.hypershift.openshift.io/dns-managed=true` to prevent future reconciliations
3. If the hosted zone is not found or AWS credentials fail:
   - Logs detailed error with specific reason (credentials, permissions, zone not found)
   - Creates a Kubernetes Warning event on the HostedCluster resource
   - Sets a condition `Route53Ready=False` with appropriate reason
4. On success:
   - Creates a Kubernetes Normal event
   - Sets condition `Route53Ready=True`
   - Adds the DNS managed label
   - Controller is never triggered again for this resource (filtered by watch predicate)
5. When a HostedCluster is deleted:
   - The watch predicate allows deletion events through even if label is set
   - The finalizer ensures Route53 records are cleaned up
   - Deletes both `api` and `api-int` ALIAS records
   - Removes the finalizer to allow the HostedCluster to be deleted

### NodePool Controller

Manages application ingress DNS records (`*.apps.*`) with automatic updates when Agents change:

1. Watches `NodePool` resources with an event filter predicate
2. **Watches `Agent` resources** to automatically update DNS when:
   - An Agent is added to the cluster
   - An Agent is removed from the cluster
   - An Agent's IP address changes
3. When a NodePool is created or updated:
   - **Watch predicate filtering**: Resources with label `route53.hypershift.openshift.io/nodepool-dns-managed=true` are filtered out
   - Deletion events always pass through the predicate to ensure proper cleanup
   - Finds the associated HostedCluster via `spec.clusterName`
   - Discovers Agent resources assigned to this cluster (searches cluster-wide, any namespace)
   - Extracts IP addresses from Agent `status.inventory.interfaces[].ipV4Addresses[]` (note: capital V)
   - Searches Route53 for the hosted zone matching the cluster's base domain
   - **Adds a finalizer** to the NodePool before creating records
   - Creates/updates multiple A records for `*.apps.<cluster-name>.<base-domain>` with all Agent IPs
   - **Adds label** `route53.hypershift.openshift.io/nodepool-dns-managed=true` to prevent future reconciliations
4. When an Agent changes:
   - Agent watch detects the change
   - Finds all NodePools for that cluster
   - Removes the DNS managed label to trigger reconciliation
   - NodePool reconciliation updates the Route53 record with new Agent IPs
5. If Agents are not ready:
   - Waits and requeues until Agent IPs become available
   - Does not create DNS records until at least one Agent IP is found
6. On success:
   - Creates a Kubernetes Normal event on the NodePool
   - Adds the DNS managed label
   - Controller is never triggered again until an Agent change occurs
7. When a NodePool is deleted:
   - The finalizer ensures the `*.apps` DNS record is deleted
   - Removes the finalizer to allow the NodePool to be deleted

> **Note**: Agent IP discovery is implemented using Kubernetes unstructured clients. See [AGENT_IMPLEMENTATION.md](AGENT_IMPLEMENTATION.md) for implementation details and optional enhancements.

## Status Conditions

The controller updates the HostedCluster status with conditions:

```yaml
status:
  conditions:
  - type: Route53Ready
    status: "True"
    lastTransitionTime: "2024-01-15T10:30:00Z"
    reason: RecordsCreated
    message: "Created Route53 A records: api.cluster1.example.com and api-int.cluster1.example.com pointing to lb-xxx.us-east-1.elb.amazonaws.com"
```

## Events

The controller generates Kubernetes events:

- `Route53RecordsCreated`: Successfully created DNS records
- `Route53RecordsDeleted`: Successfully deleted DNS records during cleanup
- `HostedZoneNotFound`: No Route53 zone found matching the base domain
- `AWSCredentialsError`: AWS credentials are missing or invalid (e.g., invalid token)
- `AWSAuthenticationError`: AWS authentication failed (e.g., invalid signature)
- `AWSAccessDenied`: AWS IAM permissions are insufficient
- `Route53Error`: Failed to create/update DNS records
- `InvalidConfiguration`: Missing required fields in HostedCluster spec

## Labels

The controller uses a label with watch predicate filtering for efficient reconciliation control:

- **`route53.hypershift.openshift.io/dns-managed=true`**: Added after successfully creating DNS records
- This label is used by the watch predicate to filter events at the controller level
- Resources with this label do not trigger reconciliation (except during deletion)
- This is more efficient than annotation-based filtering as the controller is never invoked
- To force DNS reconfiguration, remove this label and the controller will reconcile again:
  ```bash
  kubectl label hostedcluster <name> -n <namespace> route53.hypershift.openshift.io/dns-managed-
  ```

## Finalizers

The controller adds a finalizer `route53.hypershift.openshift.io/dns-records` to each HostedCluster before creating DNS records. This ensures proper cleanup when the cluster is deleted:

- The finalizer is added just before creating Route53 records
- When a HostedCluster is deleted, the controller deletes the DNS records before removing the finalizer
- This prevents orphaned DNS records in Route53

## Troubleshooting

### Check controller logs

```bash
kubectl logs -n hypershift deployment/route53-controller
```

### Check events on a HostedCluster

```bash
kubectl describe hostedcluster -n <namespace> <cluster-name>
```

### Verify AWS credentials

```bash
kubectl exec -n hypershift deployment/route53-controller -- env | grep AWS
```

### Common Issues

#### AWS CLI Connection Issues

If `aws route53 list-hosted-zones` fails:

**"Unable to locate credentials"**
```bash
# Check if AWS CLI is configured
aws configure list

# If empty, configure it:
aws configure

# Or set environment variables:
export AWS_ACCESS_KEY_ID="your-key"
export AWS_SECRET_ACCESS_KEY="your-secret"
export AWS_DEFAULT_REGION="us-east-1"
```

**"Could not connect to the endpoint URL"**
```bash
# Check internet connectivity
ping route53.amazonaws.com

# Verify region is correct
aws configure get region

# Try with explicit region
aws route53 list-hosted-zones --region us-east-1
```

**"An error occurred (InvalidClientTokenId)"**
```bash
# Access key is invalid - regenerate it:
aws iam create-access-key --user-name route53-controller

# Update your credentials
aws configure
```

#### Controller Runtime Issues

1. **AWSCredentialsError - "The security token included in the request is invalid"**:
   - This means AWS credentials are not configured or are invalid in the pod
   - Check that the ServiceAccount has the correct IAM role annotation (for IRSA)
   - Verify AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY environment variables (if using static credentials)
   - Check the credentials secret is mounted correctly
   - Test with: `kubectl exec -n hypershift deployment/route53-controller -- env | grep AWS`

2. **AWSAccessDenied - "403 Forbidden"**:
   - The IAM role/user lacks necessary Route53 permissions
   - Verify the IAM policy includes: `route53:ListHostedZones`, `route53:ChangeResourceRecordSets`, `route53:ListResourceRecordSets`

3. **No hosted zone found**:
   - Ensure you have a Route53 hosted zone that matches part of the base domain
   - The controller looks for the longest matching zone (e.g., for `cluster.example.com`, it will match a zone named `example.com`)

4. **Load balancer DNS not available**:
   - The controller will wait and requeue until the LB is provisioned in `status.controlPlaneEndpoint.host`

5. **DNS not updating after manual changes**:
   - If you manually modified Route53 records and want the controller to recreate them, remove the `route53.hypershift.openshift.io/dns-managed` label:
   ```bash
   kubectl label hostedcluster <name> -n <namespace> route53.hypershift.openshift.io/dns-managed-
   ```

## Development

```bash
# Format code
make fmt

# Run linter
make vet

# Run tests
make test

# Run locally (requires kubeconfig)
make run
```

## License

Apache 2.0
