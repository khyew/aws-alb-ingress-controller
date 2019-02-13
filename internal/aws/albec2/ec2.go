package albec2

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"

	"github.com/aws/aws-sdk-go/service/elbv2"

	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws/albrgt"
	"github.com/kubernetes-sigs/aws-alb-ingress-controller/pkg/util/log"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"

	"github.com/kubernetes-sigs/aws-alb-ingress-controller/internal/aws/albcache"
	util "github.com/kubernetes-sigs/aws-alb-ingress-controller/pkg/util/types"
)

const (
	instSpecifierTag = "instance"
	ManagedByKey     = "ManagedBy"
	ManagedByValue   = "alb-ingress"

	tagNameCluster = "kubernetes.io/cluster"

	tagNameSubnetInternalELB = "kubernetes.io/role/internal-elb"
	tagNameSubnetPublicELB   = "kubernetes.io/role/elb"

	GetSecurityGroupsCacheTTL = time.Minute * 60
	GetSubnetsCacheTTL        = time.Minute * 60

	IsNodeHealthyCacheTTL = time.Minute * 5
)

// EC2svc is a pointer to the awsutil EC2 service
var EC2svc *EC2

// EC2Metadatasvc is a pointer to the awsutil EC2metadata service
var EC2Metadatasvc *EC2MData

// EC2 is our extension to AWS's ec2.EC2
type EC2 struct {
	ec2iface.EC2API
}

// EC2MData is our extension to AWS's ec2metadata.EC2Metadata
// cache is not required for this struct as we only use it to lookup
// instance metadata when the cache for the EC2 struct is expired.
type EC2MData struct {
	*ec2metadata.EC2Metadata
}

// NewEC2 returns an awsutil EC2 service
func NewEC2(awsSession *session.Session) {
	EC2svc = &EC2{
		ec2.New(awsSession),
	}
}

// NewEC2Metadata returns an awsutil EC2Metadata service
func NewEC2Metadata(awsSession *session.Session) {
	EC2Metadatasvc = &EC2MData{
		ec2metadata.New(awsSession),
	}
}

func (e *EC2) GetSubnets(names []*string) (subnets []*string, err error) {
	vpcID, err := EC2svc.GetVPCID()
	if err != nil {
		return
	}

	cacheName := "EC2.GetSubnets"
	var queryNames []*string

	for _, n := range names {
		item := albcache.Get(cacheName, *n)

		if item != nil {
			subnets = append(subnets, item.Value().(*string))
		} else {
			queryNames = append(queryNames, n)
		}
	}

	if len(queryNames) == 0 {
		return
	}

	in := &ec2.DescribeSubnetsInput{Filters: []*ec2.Filter{
		{
			Name:   aws.String("tag:Name"),
			Values: names,
		},
		{
			Name:   aws.String("vpc-id"),
			Values: []*string{vpcID},
		},
	}}

	describeSubnetsOutput, err := EC2svc.DescribeSubnets(in)
	if err != nil {
		return subnets, fmt.Errorf("Unable to fetch subnets %v: %v", in.Filters, err)
	}

	for _, subnet := range describeSubnetsOutput.Subnets {
		value, ok := util.EC2Tags(subnet.Tags).Get("Name")
		if ok {
			albcache.Set(cacheName, value, subnet.SubnetId, GetSubnetsCacheTTL)
			subnets = append(subnets, subnet.SubnetId)
		}
	}
	return
}

func (e *EC2) GetSecurityGroups(names []*string) (sgs []*string, err error) {
	vpcID, err := EC2svc.GetVPCID()
	if err != nil {
		return
	}

	cacheName := "EC2.GetSecurityGroups"
	var queryNames []*string

	for _, n := range names {
		item := albcache.Get(cacheName, *n)

		if item != nil {
			sgs = append(sgs, item.Value().(*string))
		} else {
			queryNames = append(queryNames, n)
		}
	}

	if len(queryNames) == 0 {
		return
	}

	in := &ec2.DescribeSecurityGroupsInput{Filters: []*ec2.Filter{
		{
			Name:   aws.String("tag:Name"),
			Values: queryNames,
		},
		{
			Name:   aws.String("vpc-id"),
			Values: []*string{vpcID},
		},
	}}

	describeSecurityGroupsOutput, err := EC2svc.DescribeSecurityGroups(in)
	if err != nil {
		return sgs, fmt.Errorf("Unable to fetch security groups %v: %v", in.Filters, err)
	}

	for _, sg := range describeSecurityGroupsOutput.SecurityGroups {
		name, _ := util.EC2Tags(sg.Tags).Get("Name")
		albcache.Set(cacheName, name, sg.GroupId, GetSecurityGroupsCacheTTL)
		sgs = append(sgs, sg.GroupId)
	}

	return
}

func (e *EC2) GetInstancesByID(instanceIDs []string) ([]*ec2.Instance, error) {
	reservations, err := e.describeInstancesHelper(&ec2.DescribeInstancesInput{
		InstanceIds: aws.StringSlice(instanceIDs),
	})
	if err != nil {
		return nil, err
	}
	var result []*ec2.Instance
	for _, reservation := range reservations {
		result = append(result, reservation.Instances...)
	}
	return result, nil
}

// GetSecurityGroupByID retrives securityGroup by ID
func (e *EC2) GetSecurityGroupByID(sgID string) (*ec2.SecurityGroup, error) {
	securityGroups, err := e.describeSecurityGroupsHelper(&ec2.DescribeSecurityGroupsInput{
		GroupIds: []*string{&sgID},
	})
	if err != nil {
		return nil, err
	}
	if len(securityGroups) == 0 {
		return nil, nil
	}
	return securityGroups[0], nil
}

// GetSecurityGroupByName retrives securityGroup by vpcID and securityGroupName(SecurityGroup names within vpc are unique)
func (e *EC2) GetSecurityGroupByName(vpcID string, sgName string) (*ec2.SecurityGroup, error) {
	securityGroups, err := e.describeSecurityGroupsHelper(&ec2.DescribeSecurityGroupsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("vpc-id"),
				Values: []*string{aws.String(vpcID)},
			},
			{
				Name:   aws.String("group-name"),
				Values: []*string{aws.String(sgName)},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	if len(securityGroups) == 0 {
		return nil, nil
	}
	return securityGroups[0], nil
}

// DeleteSecurityGroupByID delete securityGroup by ID
func (e *EC2) DeleteSecurityGroupByID(sgID string) error {
	input := &ec2.DeleteSecurityGroupInput{
		GroupId: aws.String(sgID),
	}

	retryOption := func(req *request.Request) {
		req.Retryer = &deleteSecurityGroupRetryer{
			req.Retryer,
		}
	}
	if _, err := e.DeleteSecurityGroupWithContext(aws.BackgroundContext(), input, retryOption); err != nil {
		return err
	}
	return nil
}

// describeSecurityGroups is an helper to handle pagination for DescribeSecurityGroups API call
func (e *EC2) describeSecurityGroupsHelper(params *ec2.DescribeSecurityGroupsInput) (results []*ec2.SecurityGroup, err error) {
	p := request.Pagination{
		EndPageOnSameToken: true,
		NewRequest: func() (*request.Request, error) {
			req, _ := e.DescribeSecurityGroupsRequest(params)
			return req, nil
		},
	}
	for p.Next() {
		page := p.Page().(*ec2.DescribeSecurityGroupsOutput)
		results = append(results, page.SecurityGroups...)
	}
	err = p.Err()
	return results, err
}

func (e *EC2) describeInstancesHelper(params *ec2.DescribeInstancesInput) (result []*ec2.Reservation, err error) {
	err = e.DescribeInstancesPages(params, func(output *ec2.DescribeInstancesOutput, _ bool) bool {
		result = append(result, output.Reservations...)
		return true
	})
	return result, err
}

// GetVPCID returns the VPC of the instance the controller is currently running on.
// This is achieved by getting the identity document of the EC2 instance and using
// the DescribeInstances call to determine its VPC ID.
func (e *EC2) GetVPCID() (*string, error) {
	var vpc *string

	if v := os.Getenv("AWS_VPC_ID"); v != "" {
		return &v, nil
	}

	// If previously looked up (and not expired) the VpcId will be stored in the cache under the
	// key 'vpc'.
	cacheName := "EC2.GetVPCID"
	item := albcache.Get(cacheName, "")

	// cache hit: return (pointer of) VpcId value
	if item != nil {
		vpc = item.Value().(*string)
		return vpc, nil
	}

	// cache miss: begin lookup of VpcId based on current EC2 instance
	// retrieve identity of current running instance
	identityDoc, err := EC2Metadatasvc.GetInstanceIdentityDocument()
	if err != nil {
		return nil, err
	}

	// capture instance ID for lookup in DescribeInstances
	// don't bother caching this value as it should never be re-retrieved unless
	// the cache for the VpcId (looked up below) expires.
	descInstancesInput := &ec2.DescribeInstancesInput{
		InstanceIds: []*string{aws.String(identityDoc.InstanceID)},
	}

	// capture description of this instance for later capture of VpcId
	descInstancesOutput, err := e.DescribeInstances(descInstancesInput)
	if err != nil {
		return nil, err
	}

	// Before attempting to return VpcId of instance, ensure at least 1 reservation and instance
	// (in that reservation) was found.
	if err = instanceVPCIsValid(descInstancesOutput); err != nil {
		return nil, err
	}

	vpc = descInstancesOutput.Reservations[0].Instances[0].VpcId
	// cache the retrieved VpcId for next call
	albcache.Set(cacheName, "", vpc, time.Minute*60)
	return vpc, nil
}

func (e *EC2) GetVPC(id *string) (*ec2.Vpc, error) {
	cacheName := "EC2.GetVPCID"
	item := albcache.Get(cacheName, *id)

	// cache hit: return (pointer of) VpcId value
	if item != nil {
		vpc := item.Value().(*ec2.Vpc)
		return vpc, nil
	}

	o, err := e.DescribeVpcs(&ec2.DescribeVpcsInput{
		VpcIds: []*string{id},
	})
	if err != nil {
		return nil, err
	}
	if len(o.Vpcs) != 1 {
		return nil, fmt.Errorf("Invalid amount of VPCs %d returned for %s", len(o.Vpcs), *id)
	}

	albcache.Set(cacheName, *id, o.Vpcs[0], time.Minute*60)
	return o.Vpcs[0], nil
}

// instanceVPCIsValid ensures returned instance data has a valid VPC ID in the output
func instanceVPCIsValid(o *ec2.DescribeInstancesOutput) error {
	if len(o.Reservations) < 1 {
		return fmt.Errorf("When looking up VPC ID could not identify instance. Found %d reservations"+
			" in AWS call. Should have found atleast 1.", len(o.Reservations))
	}
	if len(o.Reservations[0].Instances) < 1 {
		return fmt.Errorf("When looking up VPC ID could not identify instance. Found %d instances"+
			" in AWS call. Should have found atleast 1.", len(o.Reservations))
	}
	if o.Reservations[0].Instances[0].VpcId == nil {
		return fmt.Errorf("When looking up VPC ID could not instance returned had a nil value for VPC.")
	}
	if *o.Reservations[0].Instances[0].VpcId == "" {
		return fmt.Errorf("When looking up VPC ID could not instance returned had an empty value for VPC.")
	}

	return nil
}

// Status validates EC2 connectivity
func (e *EC2) Status() func() error {
	return func() error {
		in := &ec2.DescribeTagsInput{}
		in.SetMaxResults(6)

		if _, err := e.DescribeTags(in); err != nil {
			return fmt.Errorf("[ec2.DescribeTags]: %v", err)
		}
		return nil
	}
}

// ClusterSubnets returns the subnets that are tagged for the cluster
func ClusterSubnets(scheme *string) (util.Subnets, error) {
	var useableSubnets []*ec2.Subnet
	var out util.AWSStringSlice
	var key string

	cacheName := "ClusterSubnets"

	if *scheme == elbv2.LoadBalancerSchemeEnumInternal {
		key = tagNameSubnetInternalELB
	} else if *scheme == elbv2.LoadBalancerSchemeEnumInternetFacing {
		key = tagNameSubnetPublicELB
	} else {
		return nil, fmt.Errorf("Invalid scheme [%s]", *scheme)
	}

	resources, err := albrgt.RGTsvc.GetClusterResources()
	if err != nil {
		return nil, fmt.Errorf("Failed to get AWS tags. Error: %s", err.Error())
	}

	var filterValues []*string
	for arn, subnetTags := range resources.Subnets {
		for _, tag := range subnetTags {
			if *tag.Key == key {
				p := strings.Split(arn, "/")
				subnetID := &p[len(p)-1]
				item := albcache.Get(cacheName, *subnetID)
				if item != nil {
					if subnetIsUsable(item.Value().(*ec2.Subnet), useableSubnets) {
						useableSubnets = append(useableSubnets, item.Value().(*ec2.Subnet))
						out = append(out, item.Value().(*ec2.Subnet).SubnetId)
					}
				} else {
					filterValues = append(filterValues, subnetID)
				}
			}
		}
	}

	if len(filterValues) == 0 {
		sort.Sort(out)
		return util.Subnets(out), nil
	}

	input := &ec2.DescribeSubnetsInput{
		Filters: []*ec2.Filter{
			&ec2.Filter{
				Name:   aws.String("subnet-id"),
				Values: filterValues,
			},
		},
	}
	o, err := EC2svc.DescribeSubnets(input)
	if err != nil {
		return nil, fmt.Errorf("Unable to fetch subnets %v: %v", log.Prettify(input.Filters), err)
	}

	for _, subnet := range o.Subnets {
		if subnetIsUsable(subnet, useableSubnets) {
			useableSubnets = append(useableSubnets, subnet)
			out = append(out, subnet.SubnetId)
			albcache.Set(cacheName, *subnet.SubnetId, subnet, time.Minute*60)
		}
	}

	if len(out) < 2 {
		return nil, fmt.Errorf("Retrieval of subnets failed to resolve 2 qualified subnets. Subnets must "+
			"contain the %s/<cluster name> tag with a value of shared or owned and the %s tag signifying it should be used for ALBs "+
			"Additionally, there must be at least 2 subnets with unique availability zones as required by "+
			"ALBs. Either tag subnets to meet this requirement or use the subnets annotation on the "+
			"ingress resource to explicitly call out what subnets to use for ALB creation. The subnets "+
			"that did resolve were %v.", tagNameCluster, tagNameSubnetInternalELB,
			log.Prettify(out))
	}

	sort.Sort(out)
	return util.Subnets(out), nil
}

// subnetIsUsable determines if the subnet shares the same availablity zone as a subnet in the
// existing list. If it does, false is returned as you cannot have albs provisioned to 2 subnets in
// the same availability zone.
func subnetIsUsable(new *ec2.Subnet, existing []*ec2.Subnet) bool {
	for _, subnet := range existing {
		if *new.AvailabilityZone == *subnet.AvailabilityZone {
			return false
		}
	}
	return true
}

// IsNodeHealthy returns true if the node is ready
func (e *EC2) IsNodeHealthy(instanceid string) (bool, error) {
	cacheName := "ec2.IsNodeHealthy"
	item := albcache.Get(cacheName, instanceid)

	if item != nil {
		return item.Value().(bool), nil
	}

	in := &ec2.DescribeInstanceStatusInput{
		InstanceIds: []*string{aws.String(instanceid)},
	}
	o, err := e.DescribeInstanceStatus(in)
	if err != nil {
		return false, fmt.Errorf("Unable to DescribeInstanceStatus on %v: %v", instanceid, err.Error())
	}

	for _, instanceStatus := range o.InstanceStatuses {
		if *instanceStatus.InstanceId != instanceid {
			continue
		}
		if *instanceStatus.InstanceState.Code == 16 { // running
			albcache.Set(cacheName, instanceid, true, IsNodeHealthyCacheTTL)
			return true, nil
		}
		albcache.Set(cacheName, instanceid, false, IsNodeHealthyCacheTTL)
		return false, nil
	}

	return false, nil
}

type deleteSecurityGroupRetryer struct {
	request.Retryer
}

func (r *deleteSecurityGroupRetryer) ShouldRetry(req *request.Request) bool {
	if awsErr, ok := req.Error.(awserr.Error); ok {
		if awsErr.Code() == "DependencyViolation" {
			return true
		}
	}
	// Fallback to built in retry rules
	return r.Retryer.ShouldRetry(req)
}

func (r *deleteSecurityGroupRetryer) MaxRetries() int {
	return 20
}
