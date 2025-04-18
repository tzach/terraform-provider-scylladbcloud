package provider

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/scylladb/terraform-provider-scylladbcloud/internal/scylla"
	"github.com/scylladb/terraform-provider-scylladbcloud/internal/scylla/model"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

const (
	clusterRetryTimeout    = 40 * time.Minute
	clusterDeleteTimeout   = 90 * time.Minute
	clusterRetryDelay      = 5 * time.Second
	clusterRetryMinTimeout = 15 * time.Second
	clusterPollInterval    = 10 * time.Second
)

func ResourceCluster() *schema.Resource {
	return &schema.Resource{
		Create: resourceClusterCreate,
		Read:   resourceClusterRead,
		Update: resourceClusterUpdate,
		Delete: resourceClusterDelete,

		Importer: &schema.ResourceImporter{
			State: schema.ImportStatePassthrough,
		},

		Timeouts: &schema.ResourceTimeout{
			Create: schema.DefaultTimeout(clusterRetryTimeout),
			Update: schema.DefaultTimeout(clusterRetryTimeout),
			Delete: schema.DefaultTimeout(clusterDeleteTimeout),
		},

		Schema: map[string]*schema.Schema{
			"cluster_id": {
				Description: "Cluster id",
				Computed:    true,
				Type:        schema.TypeInt,
			},
			"name": {
				Description: "Cluster name",
				Required:    true,
				ForceNew:    true,
				Type:        schema.TypeString,
			},
			"region": {
				Description: "Region to use",
				Required:    true,
				ForceNew:    true,
				Type:        schema.TypeString,
			},
			"node_count": {
				Description: "Node count",
				Required:    true,
				ForceNew:    true,
				Type:        schema.TypeInt,
			},
			"user_api_interface": {
				Description: "Type of API interface, either CQL or ALTERNATOR",
				Optional:    true,
				ForceNew:    true,
				Type:        schema.TypeString,
				Default:     "CQL",
			},
			"alternator_write_isolation": {
				Description: "Default write isolation policy",
				Optional:    true,
				ForceNew:    true,
				Type:        schema.TypeString,
				Default:     "only_rmw_uses_lwt",
			},
			"node_type": {
				Description: "Instance type of a node",
				Required:    true,
				ForceNew:    true,
				Type:        schema.TypeString,
			},
			"cidr_block": {
				Description: "IPv4 CIDR of the cluster",
				Optional:    true,
				Computed:    true,
				ForceNew:    true,
				Type:        schema.TypeString,
			},
			"scylla_version": {
				Description: "Scylla version",
				Optional:    true,
				Computed:    true,
				ForceNew:    true,
				Type:        schema.TypeString,
			},
			"enable_vpc_peering": {
				Description: "Whether to enable VPC peering",
				Optional:    true,
				ForceNew:    true,
				Type:        schema.TypeBool,
				Default:     true,
			},
			"enable_dns": {
				Description: "Whether to enable CNAME for seed nodes",
				Optional:    true,
				Type:        schema.TypeBool,
				// NOTE(rjeczalik): ForceNew is commented out here, otherwise
				// internal provider validate fails due to all the attrs
				// being ForceNew; Scylla Cloud API does not allow for
				// updating existing clusters, thus update the implementation
				// always returns a non-nil error.
				// ForceNew:    true,
				Default: true,
			},
			"request_id": {
				Description: "Cluster creation request ID",
				Computed:    true,
				Type:        schema.TypeInt,
			},
			"datacenter": {
				Description: "Cluster datacenter name",
				Computed:    true,
				Type:        schema.TypeString,
			},
			"status": {
				Description: "Cluster status",
				Computed:    true,
				Type:        schema.TypeString,
			},
		},
	}
}

func resourceClusterCreate(d *schema.ResourceData, meta interface{}) error {
	var (
		c = meta.(*scylla.Client)
		r = &model.ClusterCreateRequest{
			AccountCredentialID:  1,
			ClusterName:          d.Get("name").(string),
			BroadcastType:        "PRIVATE",
			ReplicationFactor:    3,
			NumberOfNodes:        int64(d.Get("node_count").(int)),
			UserAPIInterface:     d.Get("user_api_interface").(string),
			EnableDNSAssociation: d.Get("enable_dns").(bool),
		}
		cidr, cidrOK       = d.GetOk("cidr_block")
		region             = d.Get("region").(string)
		nodeType           = d.Get("node_type").(string)
		version, versionOK = d.GetOk("scylla_version")
		enableVpcPeering   = d.Get("enable_vpc_peering").(bool)
	)

	if !enableVpcPeering {
		r.BroadcastType = "PUBLIC"
	}

	if r.UserAPIInterface == "ALTERNATOR" {
		r.AlternatorWriteIsolation = d.Get("alternator_write_isolation").(string)
	}

	if !cidrOK {
		cidr = "172.31.0.0/16"
		d.Set("cidr_block", cidr)
	}

	r.CidrBlock = cidr.(string)

	r.CloudProviderID = c.Meta.AWS.CloudProvider.ID

	if mr := c.Meta.AWS.RegionByName(region); mr != nil {
		r.RegionID = mr.ID
	} else {
		return fmt.Errorf(`unrecognized value %q for "region" attribute`, region)
	}

	if mi := c.Meta.AWS.InstanceByName(nodeType); mi != nil {
		r.InstanceID = mi.ID
	} else {
		return fmt.Errorf(`unrecognized value %q for "node_type" attribute`, nodeType)
	}

	if defaultID := c.Meta.ScyllaVersions.DefaultScyllaVersionID; !versionOK {
		r.ScyllaVersionID = c.Meta.ScyllaVersions.DefaultScyllaVersionID
		d.Set("scylla_version", c.Meta.VersionByID(defaultID).Version)
	} else if mv := c.Meta.VersionByName(version.(string)); mv != nil {
		r.ScyllaVersionID = mv.VersionID
	} else {
		return fmt.Errorf(`unrecognized value %q for "scylla_version" attribute`, version)
	}

	cr, err := c.CreateCluster(r)
	if err != nil {
		return fmt.Errorf("error creating cluster: %w", err)
	}

	d.SetId(strconv.Itoa(int(cr.ClusterID)))
	d.Set("cluster_id", cr.ClusterID)
	d.Set("request_id", cr.ID)

	if err := waitForCluster(c, cr.ID); err != nil {
		return fmt.Errorf("error waiting for cluster: %w", err)
	}

	cluster, err := c.GetCluster(cr.ClusterID)
	if err != nil {
		return fmt.Errorf("error reading cluster: %w", err)
	}

	d.Set("datacenter_id", cluster.Datacenter.ID)

	return nil
}

func resourceClusterRead(d *schema.ResourceData, meta interface{}) error {
	var (
		c = meta.(*scylla.Client)
	)

	clusterID, err := strconv.ParseInt(d.Id(), 10, 64)
	if err != nil {
		return fmt.Errorf("error reading id=%q: %w", d.Id(), err)
	}

	reqs, err := c.ListClusterRequest(clusterID, "CREATE_CLUSTER")
	if err != nil {
		return fmt.Errorf("error reading cluster request: %w", err)
	}
	if len(reqs) != 1 {
		return fmt.Errorf("unexpected number of cluster requests, expected 1, got: %+v", reqs)
	}

	if reqs[0].Status != "COMPLETED" {
		if err := waitForCluster(c, reqs[0].ID); err != nil {
			return fmt.Errorf("error waiting for cluster: %w", err)
		}
	}

	cluster, err := c.GetCluster(clusterID)
	if err != nil {
		return fmt.Errorf("error reading cluster: %w", err)
	}

	if n := len(cluster.Datacenters); n > 1 {
		return fmt.Errorf("multi-datacenter clusters are not currently supported: %d", n)
	}

	d.Set("cluster_id", cluster.ID)
	d.Set("name", cluster.ClusterName)
	d.Set("region", cluster.Region.ExternalID)
	d.Set("node_count", len(model.NodesByStatus(cluster.Nodes, "ACTIVE")))
	d.Set("user_api_interface", cluster.UserAPIInterface)
	d.Set("node_type", c.Meta.AWS.InstanceByID(cluster.Datacenter.InstanceID).ExternalID)
	d.Set("cidr_block", cluster.Datacenter.CIDRBlock)
	d.Set("scylla_version", cluster.ScyllaVersion.Version)
	d.Set("enable_vpc_peering", !strings.EqualFold(cluster.BroadcastType, "PUBLIC"))
	d.Set("enable_dns", cluster.DNS)
	d.Set("request_id", reqs[0].ID)
	d.Set("datacenter", cluster.Datacenter.Name)
	d.Set("status", cluster.Status)

	return nil
}

func resourceClusterUpdate(d *schema.ResourceData, meta interface{}) error {
	// Scylla Cloud API does not support updating a cluster,
	// thus the update always fails
	return fmt.Errorf(`updating "scylla_cluster" resource is not supported`)
}

func resourceClusterDelete(d *schema.ResourceData, meta interface{}) error {
	var (
		c = meta.(*scylla.Client)
	)

	clusterID, err := strconv.ParseInt(d.Id(), 10, 64)
	if err != nil {
		return fmt.Errorf("error reading id=%q: %w", d.Id(), err)
	}

	name, ok := d.GetOk("name")
	if !ok {
		return fmt.Errorf("unable to read cluster name from state file")
	}

	r, err := c.DeleteCluster(clusterID, name.(string))
	if err != nil {
		return fmt.Errorf("error deleting cluster: %w", err)
	}

	if !strings.EqualFold(r.Status, "QUEUED") && !strings.EqualFold(r.Status, "IN_PROGRESS") {
		return fmt.Errorf("delete request failure: %q", r.UserFriendlyError)
	}

	return nil
}

func waitForCluster(c *scylla.Client, requestID int64) error {
	t := time.NewTicker(clusterPollInterval)
	defer t.Stop()

	for range t.C {
		r, err := c.GetClusterRequest(requestID)
		if err != nil {
			return fmt.Errorf("error reading cluster request: %w", err)
		}

		if strings.EqualFold(r.Status, "COMPLETED") {
			break
		} else if strings.EqualFold(r.Status, "QUEUED") || strings.EqualFold(r.Status, "IN_PROGRESS") {
			continue
		}

		return fmt.Errorf("unrecognized cluster request status: %q", r.Status)
	}

	return nil
}
