package cm

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"

	common "github.com/ThalesGroup/terraform-provider-ciphertrust/internal/provider/common"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

var (
	_ resource.Resource              = &resourceCMCluster{}
	_ resource.ResourceWithConfigure = &resourceCMCluster{}
)

func NewResourceCMCluster() resource.Resource {
	return &resourceCMCluster{}
}

type resourceCMCluster struct {
	client *common.Client
}

func (r *resourceCMCluster) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_cluster"
}

// Schema defines the schema for the resource.
func (r *resourceCMCluster) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"nodes": schema.ListNestedAttribute{
				Optional: true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"host": schema.StringAttribute{
							Required:    true,
							Description: "The hostname or IP of the node",
						},
						"original": schema.BoolAttribute{
							Optional:    true,
							Description: "This node is the same server as the provider is configured to use. It is used as the first node in the cluster. All other nodes will have the same state as this node.",
						},
						"port": schema.Int64Attribute{
							Required:    true,
							Description: "The port of the node, typically 5432",
						},
						"public_address": schema.StringAttribute{
							Required:    true,
							Description: "The fully qualified domain name (FQDN) or public IP of this node. This attribute is used by CipherTrust Manager connectors to learn how to access this particular node of the cluster remotely.",
						},
						"credentials": schema.SingleNestedAttribute{
							Optional:    true,
							Description: "Credentials for the node that want to join the cluster. This is optional and if not provided, provider's config node credentials shall be tried.",
							Attributes: map[string]schema.Attribute{
								"username": schema.StringAttribute{
									Optional:    true,
									Description: "Username for the node",
								},
								"password": schema.StringAttribute{
									Optional:    true,
									Description: "Password for the node",
								},
								"domain": schema.StringAttribute{
									Optional:    true,
									Description: "CipherTrust domain to log in to. Default is the empty string (root domain).",
								},
								"auth_domain": schema.StringAttribute{
									Optional:    true,
									Description: "CipherTrust authentication domain of the user. This is the domain where the user was created.",
								},
								"no_ssl_verify": &schema.BoolAttribute{
									Optional:    true,
									Description: "Set to false to verify the server's certificate chain and host name.",
								},
							},
						},
					},
				},
			},
			"node_count": schema.Int64Attribute{
				Computed:    true,
				Description: "Number of nodes in the cluster",
			},
			"node_id": schema.StringAttribute{
				Computed:    true,
				Description: "This CipherTrust manager node ID",
			},
			"status_code": schema.StringAttribute{
				Computed:    true,
				Description: "short code for cluster status: r == ready",
			},
			"status_description": schema.StringAttribute{
				Computed:    true,
				Description: "cluster status",
			},
		},
	}
}

// Create creates the resource and sets the initial Terraform state.
func (r *resourceCMCluster) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	id := uuid.New().String()
	tflog.Trace(ctx, common.MSG_METHOD_START+"[resource_cluster.go -> Create]["+id+"]")

	// Retrieve values from plan
	var plan CMClusterTFSDK

	diags := req.Plan.Get(ctx, &plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// find the original node's host and port for use as MemberNodeHost/Port in join requests
	var originalNodeHost string
	var originalNodePort int64
	for _, n := range plan.Nodes {
		if n.Original.ValueBool() {
			var err error
			originalNodeHost, err = extractHost(n.Host.ValueString())
			if err != nil {
				resp.Diagnostics.AddError("Invalid host for original node", err.Error())
				return
			}
			originalNodePort = n.Port.ValueInt64()
			break
		}
	}

	for _, node := range plan.Nodes {
		if node.Original.ValueBool() {
			//Let's check if the cluster already exists for the primary node
			response, err := r.client.ReadDataByParam(ctx, id, "all", common.URL_CLUSTER)
			if err != nil {
				tflog.Debug(ctx, common.ERR_METHOD_END+err.Error()+" [resource_cluster.go -> Create]["+id+"]")
				resp.Diagnostics.AddError(
					"Error reading existing cluster information from CipherTrust Manager: ",
					"Could not read cluster information, unexpected error: "+err.Error(),
				)
				return
			}
			statusCode := gjson.Get(response, "status.code").String()
			if statusCode == "none" {
				//This means we need to create a new cluster with the primary node
				var newClusterPayload NewCMClusterNodeJSON
				if node.Host.ValueString() != "" && node.Host.ValueString() != types.StringNull().ValueString() {
					host, err := extractHost(node.Host.ValueString())
					if err != nil {
						resp.Diagnostics.AddError("Invalid host for original node", err.Error())
						return
					}
					newClusterPayload.LocalNodeHost = host
				}
				if node.Port.ValueInt64() != types.Int64Null().ValueInt64() {
					newClusterPayload.LocalNodePort = node.Port.ValueInt64()
				}
				if node.PublicAddress.ValueString() != "" && node.PublicAddress.ValueString() != types.StringNull().ValueString() {
					host, err := extractHost(node.PublicAddress.ValueString())
					if err != nil {
						resp.Diagnostics.AddError("Invalid public_address for original node", err.Error())
						return
					}
					newClusterPayload.PublicAddress = host
				}

				payloadJSON, err := json.Marshal(newClusterPayload)
				if err != nil {
					tflog.Debug(ctx, common.ERR_METHOD_END+err.Error()+" [resource_cluster.go -> Create]["+id+"]")
					resp.Diagnostics.AddError(
						"Invalid payload: Create New Cluster",
						err.Error(),
					)
					return
				}

				response, err := r.client.PostDataV2(ctx, id, common.URL_NEW_CLUSTER, payloadJSON)
				if err != nil {
					tflog.Debug(ctx, common.ERR_METHOD_END+err.Error()+" [resource_cluster.go -> Create]["+id+"]")
					resp.Diagnostics.AddError(
						"Error creating new cluster on CipherTrust Manager: ",
						"Could not create new cluster, unexpected error: "+err.Error(),
					)
					return
				}
				plan.NodeCount = types.Int64Value(gjson.Get(response, "nodeCount").Int())
				plan.ID = types.StringValue(gjson.Get(response, "nodeID").String())
				plan.NodeId = types.StringValue(gjson.Get(response, "nodeID").String())
				plan.StatusCode = types.StringValue(gjson.Get(response, "status.code").String())
				plan.StatusDescription = types.StringValue(gjson.Get(response, "status.description").String())

				// Wait for primary node services to restart after cluster creation
				if !r.waitForServicesStarted(ctx, r.client, id, &resp.Diagnostics) {
					return
				}
			} else {
				// The cluster already exists on the primary node, update the plan with existing state
				plan.NodeCount = types.Int64Value(gjson.Get(response, "nodeCount").Int())
				plan.ID = types.StringValue(gjson.Get(response, "nodeID").String())
				plan.NodeId = types.StringValue(gjson.Get(response, "nodeID").String())
				plan.StatusCode = types.StringValue(gjson.Get(response, "status.code").String())
				plan.StatusDescription = types.StringValue(gjson.Get(response, "status.description").String())
			}
		} else {
			//Let's join remaining nodes to the cluster
			//Steps are -
			//1. Create CSR on the node that wants to join the cluster
			//For this step we need to create a client object for the joining node

			// Extract host and public_address once for this joining node.
			// Both fields accept a bare hostname/IP or a full URL (https://...).
			joiningNodeHost, err := extractHost(node.Host.ValueString())
			if err != nil {
				resp.Diagnostics.AddError("Invalid host for joining node", err.Error())
				return
			}
			joiningNodePubAddress, err := extractHost(node.PublicAddress.ValueString())
			if err != nil {
				resp.Diagnostics.AddError("Invalid public_address for joining node", err.Error())
				return
			}

			node_username := node.Creds.Username.ValueString()
			node_password := node.Creds.Password.ValueString()
			node_domain := node.Creds.Domain.ValueString()
			node_auth_domain := node.Creds.AuthDomain.ValueString()
			nodeURL := "https://" + joiningNodeHost
			node_client, err := common.NewClient(ctx, id, &nodeURL, &node_auth_domain, &node_domain, &node_username, &node_password, true, 180)
			if err != nil {
				tflog.Debug(ctx, common.ERR_METHOD_END+err.Error()+" [resource_cluster.go -> Create]["+id+"]")
				resp.Diagnostics.AddError(
					"Unable to create the HTTPS client for the joining node.",
					err.Error(),
				)
				return
			}

			var payloadCSR NewCSRJSON
			payloadCSR.LocalNodeHost = joiningNodeHost
			payloadCSR.PublicAddress = joiningNodePubAddress

			payloadCSRJSON, err := json.Marshal(payloadCSR)
			if err != nil {
				tflog.Debug(ctx, common.ERR_METHOD_END+err.Error()+" [resource_cluster.go -> Create]["+id+"]")
				resp.Diagnostics.AddError(
					"Invalid payload: Create CSR",
					err.Error(),
				)
				return
			}
			responseCSR, err := node_client.PostDataV2(ctx, id, common.URL_CREATE_CSR, payloadCSRJSON)
			if err != nil {
				tflog.Debug(ctx, common.ERR_METHOD_END+err.Error()+" [resource_cluster.go -> Create]["+id+"]")
				resp.Diagnostics.AddError(
					"Error creating new CSR on the joining node: ", err.Error(),
				)
				return
			}

			//2. Sign the CSR from an existing node in the cluster
			var payloadSignCSR SignRequestJSON
			payloadSignCSR.CSR = gjson.Get(responseCSR, "csr").String()
			payloadSignCSR.NewNodeHost = joiningNodeHost
			payloadSignCSR.PublicAddress = joiningNodePubAddress
			payloadSignCSR.SharedHSMPartition = false
			payloadSignCSRJSON, err := json.Marshal(payloadSignCSR)
			if err != nil {
				tflog.Debug(ctx, common.ERR_METHOD_END+err.Error()+" [resource_cluster.go -> Create]["+id+"]")
				resp.Diagnostics.AddError(
					"Invalid payload: Sign CSR",
					err.Error(),
				)
				return
			}

			// Retry signing the CSR if consensus is temporarily down after a node join.
			// This is needed for n-node clusters where consensus needs time to stabilize
			// after each new member joins before the next CSR can be signed.
			maxSignRetries := 30
			signRetryInterval := 10 * time.Second
			var responseSignCSR string
			for attempt := 1; attempt <= maxSignRetries; attempt++ {
				responseSignCSR, err = r.client.PostDataV2(ctx, id, common.URL_SIGN_CERT, payloadSignCSRJSON)
				if err == nil {
					break
				}
				if strings.Contains(err.Error(), "consensus is down") && attempt < maxSignRetries {
					tflog.Debug(ctx, fmt.Sprintf("[resource_cluster.go -> Create][%s] Consensus not ready, retrying sign CSR (attempt %d/%d)...", id, attempt, maxSignRetries))
					time.Sleep(signRetryInterval)
					continue
				}
				tflog.Debug(ctx, common.ERR_METHOD_END+err.Error()+" [resource_cluster.go -> Create]["+id+"]")
				resp.Diagnostics.AddError(
					"Error signing CSR on the existing node: ", err.Error(),
				)
				return
			}
			if err != nil {
				tflog.Debug(ctx, common.ERR_METHOD_END+err.Error()+" [resource_cluster.go -> Create]["+id+"]")
				resp.Diagnostics.AddError(
					"Error signing CSR on the existing node: ", err.Error(),
				)
				return
			}

			//3. Now send the request to join the cluster with the returned certificate and CA chain
			var payloadJoinNode JoinClusterJSON
			payloadJoinNode.CAChain = gjson.Get(responseSignCSR, "cachain").String()
			payloadJoinNode.Cert = gjson.Get(responseSignCSR, "cert").String()
			payloadJoinNode.MKEKBlob = gjson.Get(responseSignCSR, "mkek_blob").String()
			payloadJoinNode.LocalNodeHost = joiningNodeHost
			payloadJoinNode.MemberNodeHost = originalNodeHost
			payloadJoinNode.MemberNodePort = originalNodePort

			if node.Port.ValueInt64() != types.Int64Null().ValueInt64() {
				payloadJoinNode.LocalNodePort = node.Port.ValueInt64()
			}
			payloadJoinNode.LocalNodePublicAddress = joiningNodePubAddress
			payloadJoinNodeJSON, err := json.Marshal(payloadJoinNode)
			if err != nil {
				tflog.Debug(ctx, common.ERR_METHOD_END+err.Error()+" [resource_cluster.go -> Create]["+id+"]")
				resp.Diagnostics.AddError(
					"Invalid payload: Join Node",
					err.Error(),
				)
				return
			}
			responseJoinNode, err := node_client.PostDataV2(ctx, id, common.URL_CLUSTER_JOIN, payloadJoinNodeJSON)
			if err != nil {
				tflog.Debug(ctx, common.ERR_METHOD_END+err.Error()+" [resource_cluster.go -> Create]["+id+"]")
				resp.Diagnostics.AddError(
					"Error signing CSR on the existing node: ", err.Error(),
				)
				return
			}
			plan.NodeCount = types.Int64Value(gjson.Get(responseJoinNode, "nodeCount").Int())
			plan.ID = types.StringValue(gjson.Get(responseJoinNode, "nodeID").String())
			plan.NodeId = types.StringValue(gjson.Get(responseJoinNode, "nodeID").String())
			plan.StatusCode = types.StringValue(gjson.Get(responseJoinNode, "status.code").String())
			plan.StatusDescription = types.StringValue(gjson.Get(responseJoinNode, "status.description").String())
		}
	}

	// After all nodes have either created or joined the cluster, wait for services to be fully started before finishing resource creation
	if !r.waitForServicesStarted(ctx, r.client, id, &resp.Diagnostics) {
		return
	}
	tflog.Trace(ctx, common.MSG_METHOD_END+"[resource_cluster.go -> Create]["+id+"]")
	diags = resp.State.Set(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
}

// Read refreshes the Terraform state with the latest data.
func (r *resourceCMCluster) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state CMClusterTFSDK
	id := uuid.New().String()

	diags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	response, err := r.client.ReadDataByParam(ctx, id, "all", common.URL_CLUSTER)
	if err != nil {
		tflog.Debug(ctx, common.ERR_METHOD_END+err.Error()+" [resource_cluster.go -> Read]["+id+"]")
		resp.Diagnostics.AddError(
			"Error reading cluster info on CipherTrust Manager: ",
			"Could not cluster info : ,"+state.ID.ValueString()+"unexpected error: "+err.Error(),
		)
		return
	}

	state.NodeCount = types.Int64Value(gjson.Get(response, "nodeCount").Int())
	state.ID = types.StringValue(gjson.Get(response, "nodeID").String())
	state.NodeId = types.StringValue(gjson.Get(response, "nodeID").String())
	state.StatusCode = types.StringValue(gjson.Get(response, "status.code").String())
	state.StatusDescription = types.StringValue(gjson.Get(response, "status.description").String())

	tflog.Trace(ctx, common.MSG_METHOD_END+"[resource_cluster.go -> Read]["+id+"]")
	// Set refreshed state
	diags = resp.State.Set(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
}

// Update updates the resource and sets the updated Terraform state on success.
func (r *resourceCMCluster) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
}

// Delete deletes the resource and removes the Terraform state on success.
func (r *resourceCMCluster) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state CMClusterTFSDK
	diags := req.State.Get(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	if state.NodeCount.ValueInt64() == 1 {
		output, err := r.client.DeleteByURL(ctx, state.NodeId.ValueString(), common.URL_CLUSTER)
		tflog.Trace(ctx, common.MSG_METHOD_END+"[resource_cluster.go -> Delete]["+state.ID.ValueString()+"]["+output+"]")
		if err != nil {
			resp.Diagnostics.AddError(
				"Error deleting cluster",
				"Could not delete cluster, unexpected error: "+err.Error(),
			)
			return
		}
	} else {
		response, err := r.client.GetAll(ctx, state.ID.ValueString(), common.URL_NODES)
		if err != nil {
			resp.Diagnostics.AddError(
				"Error getting cluster nodes",
				"Could not get cluster nodes, unexpected error: "+err.Error(),
			)
			return
		}

		nodes := gjson.Parse(response).Array()
		for _, node := range nodes {
			nodeId := node.Get("id").String()
			if nodeId != "" {
				if nodeId != state.NodeId.ValueString() {
					// For non-primary nodes, we can directly delete the node from the cluster
					nodeUrl := fmt.Sprintf("%s/%s", common.URL_NODES, nodeId)
					output, err := r.client.DeleteByURL(ctx, state.NodeId.ValueString(), nodeUrl)
					tflog.Trace(ctx, common.MSG_METHOD_END+"[resource_cluster.go -> Delete node]["+nodeId+"]["+output+"]")
					if err != nil {
						resp.Diagnostics.AddError(
							"Error deleting node",
							fmt.Sprintf("Could not delete node %s, unexpected error: %s", nodeId, err.Error()),
						)
						return
					}

				}
			}
		}
	}
	// Wait for the primary node to be ready after deletion or removal of other nodes before proceeding with checks and cleanup
	if !r.waitForServicesStarted(ctx, r.client, state.ID.ValueString(), nil) {
		return
	}

	// Check if all nodes have been deleted
	response, err := r.client.GetAll(ctx, state.ID.ValueString(), common.URL_NODES)
	if err != nil {
		resp.Diagnostics.AddError(
			"Error getting cluster nodes",
			"Could not get cluster nodes, unexpected error: "+err.Error(),
		)
		return
	}
	nodes := gjson.Parse(response).Array()
	if len(nodes) > 0 {
		resp.Diagnostics.AddError(
			"Error deleting cluster",
			fmt.Sprintf("Cluster still has %d nodes after deletion attempts", len(nodes)),
		)
		return
	}
	// Remove the cluster configuration from the removed nodes
	for _, node := range nodes {
		nodeId := node.Get("id").String()

		// Create a client for the node being deleted before we delete it
		deletedNodeHost := node.Get("host").String()
		deletedNodeURL := "https://" + deletedNodeHost
		deletedNodeClient, _ := common.NewClient(ctx, nodeId, &deletedNodeURL, &r.client.AuthData.AuthDomain, &r.client.AuthData.Domain, &r.client.AuthData.Username, &r.client.AuthData.Password, true, 180)

		output, err := deletedNodeClient.DeleteByURL(ctx, state.NodeId.ValueString(), common.URL_CLUSTER)
		tflog.Trace(ctx, common.MSG_METHOD_END+"[resource_cluster.go -> Delete node config]["+nodeId+"]["+output+"]")
		if err != nil {
			resp.Diagnostics.AddError(
				"Error deleting node configuration",
				fmt.Sprintf("Could not delete node configuration for node %s, unexpected error: %s", nodeId, err.Error()),
			)
			return
		}
		// Wait for the deleted node to restart after dismantling the cluster
		if !r.waitForServicesStarted(ctx, deletedNodeClient, state.ID.ValueString(), &resp.Diagnostics) {
			return
		}
	}

	tflog.Trace(ctx, common.MSG_METHOD_END+"[resource_cluster.go -> Delete]["+state.ID.ValueString()+"]") // Remove resource from state
	resp.State.RemoveResource(ctx)

}

func (d *resourceCMCluster) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	client, ok := req.ProviderData.(*common.Client)
	if !ok {
		resp.Diagnostics.AddError(
			"Error in fetching client from provider",
			fmt.Sprintf("Expected *provider.Client, got: %T. Please report this issue to the provider developers.", req.ProviderData),
		)

		return
	}

	d.client = client
}

// extractHost extracts the bare hostname or IP from either a plain hostname/IP
// string or a URL string (e.g. "https://1.2.3.4/" or "https://cm.example.com").
// Both IP addresses and FQDNs are accepted as bare values.
func extractHost(input string) (string, error) {
	if strings.Contains(input, "://") {
		u, err := url.Parse(input)
		if err != nil {
			return "", fmt.Errorf("invalid URL %q: %w", input, err)
		}
		return u.Hostname(), nil
	}
	// Bare IP address.
	if net.ParseIP(input) != nil {
		return input, nil
	}
	// Bare hostname — non-empty is sufficient; the API will reject invalid values.
	if input != "" {
		return input, nil
	}
	return "", fmt.Errorf("host must not be empty")
}

func (r *resourceCMCluster) waitForServicesStarted(ctx context.Context, client *common.Client, id string, diags *diag.Diagnostics) bool {
	tflog.Trace(ctx, common.MSG_METHOD_START+"[resource_cm_cluster.go -> waitForServicesStarted]["+id+"]")
	defer tflog.Trace(ctx, common.MSG_METHOD_END+"[resource_cm_cluster.go -> waitForServicesStarted]["+id+"]")
	var (
		err      error
		response string
	)

	// Give the appliance a moment to initiate the service restart or system reset.
	// If we check immediately, the API might still return the pre-reboot "started" state.
	tflog.Debug(ctx, "Waiting 10 seconds for services to begin restarting...")
	time.Sleep(10 * time.Second)

	// Optional initial check (can silently fail if node is already rebooting)
	tflog.Debug(ctx, "Attempting to read initial services status")
	response, err = client.ReadDataByParam(ctx, id, "all", common.URL_SERVICES_STATUS)
	if err != nil {
		tflog.Debug(ctx, fmt.Sprintf("Error reading initial services status: %s", err.Error()))
	}

	status := gjson.Get(response, "status").String()

	numRetries := 360 // 30 minutes at 5 seconds per retry
	tStart := time.Now()
	for retry := 0; retry < numRetries && status != "started"; retry++ {
		time.Sleep(5 * time.Second)
		if time.Since(tStart).Seconds() > 200 {
			if err = client.RefreshToken(ctx, id); err != nil {
				tflog.Debug(ctx, fmt.Sprintf("Error refreshing authentication token (CM might be restarting): %s", err.Error()))
			}
			tStart = time.Now()
		}

		tflog.Debug(ctx, fmt.Sprintf("Attempting to read services status (retry %d)", retry))
		response, err = client.ReadDataByParam(ctx, id, "all", common.URL_SERVICES_STATUS)
		if err != nil {
			// If we receive a 401 Unauthorized, the node has likely reset its credentials
			// (like when a node is removed from a cluster) and the old token is rejected.
			// This is a reliable indication that the web server services are back up.
			if strings.Contains(err.Error(), "status: 401") {
				status = "started"
				break
			}
			tflog.Trace(ctx, fmt.Sprintf("Error getting services status, retrying: %s", err.Error()))
			continue
		}

		status = gjson.Get(response, "status").String()
		tflog.Trace(ctx, fmt.Sprintf("Services status: %s", status))
	}

	if status != "started" {
		msg := fmt.Sprintf("Error waiting for services: status is still '%s' after 30 minutes.", status)
		tflog.Warn(ctx, msg)
		if diags != nil {
			diags.AddError("Timeout waiting for CipherTrust Manager", msg)
		}
		return false
	}

	tflog.Trace(ctx, "[resource_cm_cluster.go -> waitForServicesStarted][response:"+response+"]")
	return true
}
