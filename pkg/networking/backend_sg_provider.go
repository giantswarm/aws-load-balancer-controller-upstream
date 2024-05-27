package networking

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	awssdk "github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	ec2sdk "github.com/aws/aws-sdk-go/service/ec2"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/aws/services"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/k8s"
	"sigs.k8s.io/aws-load-balancer-controller/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultSGDeletionPollInterval = 2 * time.Second
	defaultSGDeletionTimeout      = 2 * time.Minute

	resourceTypeSecurityGroup = "security-group"
	tagKeyK8sCluster          = "elbv2.k8s.aws/cluster"
	tagKeyResource            = "elbv2.k8s.aws/resource"
	tagValueBackend           = "backend-sg"

	explicitGroupFinalizerPrefix = "group.ingress.k8s.aws/"
	implicitGroupFinalizer       = "ingress.k8s.aws/resources"
	serviceFinalizer             = "service.k8s.aws/resources"

	sgDescription = "[k8s] Shared Backend SecurityGroup for LoadBalancer"
)

type ResourceType string

const (
	ResourceTypeIngress = "ingress"
	ResourceTypeService = "service"
)

// BackendSGProvider is responsible for providing backend security groups
type BackendSGProvider interface {
	// Get returns the backend security group to use
	Get(ctx context.Context, resourceType ResourceType, activeResources []types.NamespacedName, additionalTags map[string]string) (string, error)
	// Release cleans up the auto-generated backend SG if necessary
	Release(ctx context.Context, resourceType ResourceType, inactiveResources []types.NamespacedName) error
}

// NewBackendSGProvider constructs a new  defaultBackendSGProvider
func NewBackendSGProvider(clusterName string, backendSG string, vpcID string,
	ec2Client services.EC2, k8sClient client.Client, defaultTags map[string]string, logger logr.Logger) *defaultBackendSGProvider {
	return &defaultBackendSGProvider{
		vpcID:       vpcID,
		clusterName: clusterName,
		backendSG:   backendSG,
		defaultTags: defaultTags,
		ec2Client:   ec2Client,
		k8sClient:   k8sClient,
		logger:      logger,
		mutex:       sync.Mutex{},

		checkIngressFinalizersFunc: func(finalizers []string) bool {
			for _, fin := range finalizers {
				if fin == implicitGroupFinalizer || strings.HasPrefix(fin, explicitGroupFinalizerPrefix) {
					return true
				}
			}
			return false
		},

		checkServiceFinalizersFunc: func(finalizers []string) bool {
			for _, fin := range finalizers {
				if fin == serviceFinalizer {
					return true
				}
			}
			return false
		},

		defaultDeletionPollInterval: defaultSGDeletionPollInterval,
		defaultDeletionTimeout:      defaultSGDeletionTimeout,
	}
}

var _ BackendSGProvider = &defaultBackendSGProvider{}

type defaultBackendSGProvider struct {
	vpcID       string
	clusterName string
	mutex       sync.Mutex

	backendSG       string
	autoGeneratedSG string
	defaultTags     map[string]string
	ec2Client       services.EC2
	k8sClient       client.Client
	logger          logr.Logger
	// objectsMap keeps track of whether the backend SG is required for any tracked resources in the cluster.
	// If any entry in the map is true, or there are resources with this controller specific finalizers which
	// haven't been tracked in the map yet, controller doesn't delete the backend SG. If the controller has
	// processed all supported resources and none of them require backend SG, i.e. the values are false in this map
	// controller deletes the backend SG.
	objectsMap sync.Map

	checkServiceFinalizersFunc func([]string) bool
	checkIngressFinalizersFunc func([]string) bool

	defaultDeletionPollInterval time.Duration
	defaultDeletionTimeout      time.Duration
}

func (p *defaultBackendSGProvider) Get(ctx context.Context, resourceType ResourceType, activeResources []types.NamespacedName, additionalTags map[string]string) (string, error) {
	if len(p.backendSG) > 0 {
		return p.backendSG, nil
	}
	// Auto generate Backend Security group, and return the id
	if err := p.allocateBackendSG(ctx, resourceType, activeResources, additionalTags); err != nil {
		p.logger.Error(err, "Failed to auto-create backend SG")
		return "", err
	}
	return p.autoGeneratedSG, nil
}

func (p *defaultBackendSGProvider) Release(ctx context.Context, resourceType ResourceType,
	inactiveResources []types.NamespacedName) error {
	if len(p.backendSG) > 0 {
		return nil
	}
	defer func() {
		for _, res := range inactiveResources {
			p.objectsMap.CompareAndDelete(getObjectKey(resourceType, res), false)
		}
	}()
	p.updateObjectsMap(ctx, resourceType, inactiveResources, false)
	p.logger.V(1).Info("release backend SG", "inactive", inactiveResources)
	if required, err := p.isBackendSGRequired(ctx); required || err != nil {
		return err
	}
	return p.releaseSG(ctx)
}

func (p *defaultBackendSGProvider) updateObjectsMap(_ context.Context, resourceType ResourceType,
	resources []types.NamespacedName, backendSGRequired bool) {
	for _, res := range resources {
		p.objectsMap.Store(getObjectKey(resourceType, res), backendSGRequired)
	}
}

func (p *defaultBackendSGProvider) isBackendSGRequired(ctx context.Context) (bool, error) {
	var requiredForAny bool
	p.objectsMap.Range(func(_, v interface{}) bool {
		if v.(bool) {
			requiredForAny = true
			return false
		}
		return true
	})
	if requiredForAny {
		return true, nil
	}
	if required, err := p.checkIngressListForUnmapped(ctx); required || err != nil {
		return required, err
	}
	if required, err := p.checkServiceListForUnmapped(ctx); required || err != nil {
		return required, err
	}
	return false, nil
}

func (p *defaultBackendSGProvider) checkIngressListForUnmapped(ctx context.Context) (bool, error) {
	ingList := &networking.IngressList{}
	if err := p.k8sClient.List(ctx, ingList); err != nil {
		return true, errors.Wrapf(err, "unable to list ingresses")
	}
	for _, ing := range ingList.Items {
		if !p.checkIngressFinalizersFunc(ing.GetFinalizers()) {
			continue
		}
		if !p.existsInObjectMap(ResourceTypeIngress, k8s.NamespacedName(&ing)) {
			return true, nil
		}
	}
	return false, nil
}

func (p *defaultBackendSGProvider) checkServiceListForUnmapped(ctx context.Context) (bool, error) {
	svcList := &corev1.ServiceList{}
	if err := p.k8sClient.List(ctx, svcList); err != nil {
		return true, errors.Wrapf(err, "unable to list services")
	}
	for _, svc := range svcList.Items {
		if !p.checkServiceFinalizersFunc(svc.GetFinalizers()) {
			continue
		}
		if !p.existsInObjectMap(ResourceTypeService, k8s.NamespacedName(&svc)) {
			return true, nil
		}
	}
	return false, nil
}

func (p *defaultBackendSGProvider) existsInObjectMap(resourceType ResourceType, resource types.NamespacedName) bool {
	if _, exists := p.objectsMap.Load(getObjectKey(resourceType, resource)); exists {
		return true
	}
	return false
}

func (p *defaultBackendSGProvider) allocateBackendSG(ctx context.Context, resourceType ResourceType, activeResources []types.NamespacedName, additionalTags map[string]string) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	p.updateObjectsMap(ctx, resourceType, activeResources, true)
	if len(p.autoGeneratedSG) > 0 {
		return nil
	}

	sgName := p.getBackendSGName()
	sgID, err := p.getBackendSGFromEC2(ctx, sgName, p.vpcID)
	if err != nil {
		return err
	}
	if len(sgID) > 1 {
		p.logger.V(1).Info("Existing SG found", "id", sgID)
		p.autoGeneratedSG = sgID
		return nil
	}

	createReq := &ec2sdk.CreateSecurityGroupInput{
		VpcId:             awssdk.String(p.vpcID),
		GroupName:         awssdk.String(sgName),
		Description:       awssdk.String(sgDescription),
		TagSpecifications: p.buildBackendSGTags(ctx, additionalTags),
	}
	p.logger.V(1).Info("creating securityGroup", "name", sgName)
	resp, err := p.ec2Client.CreateSecurityGroupWithContext(ctx, createReq)
	if err != nil {
		return err
	}
	p.logger.Info("created SecurityGroup", "name", sgName, "id", resp.GroupId)
	p.autoGeneratedSG = awssdk.StringValue(resp.GroupId)
	return nil
}

func (p *defaultBackendSGProvider) buildBackendSGTags(_ context.Context, additionalTags map[string]string) []*ec2sdk.TagSpecification {
	var tags []*ec2sdk.Tag
	for key, val := range p.defaultTags {
		tags = append(tags, &ec2sdk.Tag{
			Key:   awssdk.String(key),
			Value: awssdk.String(val),
		})
	}

	for key, val := range additionalTags {
		tags = append(tags, &ec2sdk.Tag{
			Key:   awssdk.String(key),
			Value: awssdk.String(val),
		})
	}

	sort.Slice(tags, func(i, j int) bool {
		return awssdk.StringValue(tags[i].Key) < awssdk.StringValue(tags[j].Key)
	})
	return []*ec2sdk.TagSpecification{
		{
			ResourceType: awssdk.String(resourceTypeSecurityGroup),
			Tags: append(tags, []*ec2sdk.Tag{
				{
					Key:   awssdk.String(tagKeyK8sCluster),
					Value: awssdk.String(p.clusterName),
				},
				{
					Key:   awssdk.String(tagKeyResource),
					Value: awssdk.String(tagValueBackend),
				},
			}...),
		},
	}
}

func (p *defaultBackendSGProvider) getBackendSGFromEC2(ctx context.Context, sgName string, vpcID string) (string, error) {
	req := &ec2sdk.DescribeSecurityGroupsInput{
		Filters: []*ec2sdk.Filter{
			{
				Name:   awssdk.String("vpc-id"),
				Values: awssdk.StringSlice([]string{vpcID}),
			},
			{
				Name:   awssdk.String(fmt.Sprintf("tag:%v", tagKeyK8sCluster)),
				Values: awssdk.StringSlice([]string{p.clusterName}),
			},
			{
				Name:   awssdk.String(fmt.Sprintf("tag:%v", tagKeyResource)),
				Values: awssdk.StringSlice([]string{tagValueBackend}),
			},
		},
	}
	p.logger.V(1).Info("Queriying existing SG", "vpc-id", vpcID, "name", sgName)
	sgs, err := p.ec2Client.DescribeSecurityGroupsAsList(ctx, req)
	if err != nil && !isEC2SecurityGroupNotFoundError(err) {
		return "", err
	}
	if len(sgs) > 0 {
		return awssdk.StringValue(sgs[0].GroupId), nil
	}
	return "", nil
}

func (p *defaultBackendSGProvider) releaseSG(ctx context.Context) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if len(p.autoGeneratedSG) == 0 {
		return nil
	}

	if required, err := p.isBackendSGRequired(ctx); required || err != nil {
		p.logger.V(1).Info("releaseSG ignore delete", "required", required, "err", err)
		return err
	}
	req := &ec2sdk.DeleteSecurityGroupInput{
		GroupId: awssdk.String(p.autoGeneratedSG),
	}
	if err := runtime.RetryImmediateOnError(p.defaultDeletionPollInterval, p.defaultDeletionTimeout, isSecurityGroupDependencyViolationError, func() error {
		_, err := p.ec2Client.DeleteSecurityGroupWithContext(ctx, req)
		return err
	}); err != nil {
		return errors.Wrap(err, "failed to delete securityGroup")
	}
	p.logger.Info("deleted securityGroup", "ID", p.autoGeneratedSG)

	p.autoGeneratedSG = ""
	return nil
}

var invalidSGNamePattern = regexp.MustCompile("[[:^alnum:]]")

func (p *defaultBackendSGProvider) getBackendSGName() string {
	sgNameHash := sha256.New()
	_, _ = sgNameHash.Write([]byte(p.clusterName))
	sgHash := hex.EncodeToString(sgNameHash.Sum(nil))
	sanitizedClusterName := invalidSGNamePattern.ReplaceAllString(p.clusterName, "")
	return fmt.Sprintf("k8s-traffic-%.232s-%.10s", sanitizedClusterName, sgHash)
}

func isSecurityGroupDependencyViolationError(err error) bool {
	var awsErr awserr.Error
	if errors.As(err, &awsErr) {
		return awsErr.Code() == "DependencyViolation"
	}
	return false
}

func isEC2SecurityGroupNotFoundError(err error) bool {
	var awsErr awserr.Error
	if errors.As(err, &awsErr) {
		return awsErr.Code() == "InvalidGroup.NotFound"
	}
	return false
}

func getObjectKey(resourceType ResourceType, resource types.NamespacedName) string {
	return string(resourceType) + "/" + resource.String()
}
