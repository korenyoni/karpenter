/*
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

package garbagecollect_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ec2"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/patrickmn/go-cache"
	"github.com/samber/lo"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	. "knative.dev/pkg/logging/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"

	coresettings "github.com/aws/karpenter-core/pkg/apis/settings"
	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	corecloudprovider "github.com/aws/karpenter-core/pkg/cloudprovider"
	"github.com/aws/karpenter-core/pkg/operator/controller"
	"github.com/aws/karpenter-core/pkg/operator/scheme"
	coretest "github.com/aws/karpenter-core/pkg/test"
	. "github.com/aws/karpenter-core/pkg/test/expectations"

	"github.com/aws/karpenter/pkg/apis"
	"github.com/aws/karpenter/pkg/apis/settings"
	"github.com/aws/karpenter/pkg/apis/v1alpha1"
	"github.com/aws/karpenter/pkg/cloudprovider"
	"github.com/aws/karpenter/pkg/controllers/machine/garbagecollect"
	"github.com/aws/karpenter/pkg/controllers/machine/link"
	"github.com/aws/karpenter/pkg/fake"
	"github.com/aws/karpenter/pkg/test"
)

var ctx context.Context
var awsEnv *test.Environment
var env *coretest.Environment
var garbageCollectController controller.Controller
var linkedMachineCache *cache.Cache
var cloudProvider *cloudprovider.CloudProvider

func TestAPIs(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "Machine")
}

var _ = BeforeSuite(func() {
	ctx = coresettings.ToContext(ctx, coretest.Settings())
	ctx = settings.ToContext(ctx, test.Settings())
	env = coretest.NewEnvironment(scheme.Scheme, coretest.WithCRDs(apis.CRDs...))
	awsEnv = test.NewEnvironment(ctx, env)

	cloudProvider = cloudprovider.New(ctx, awsEnv.InstanceTypesProvider, awsEnv.InstanceProvider, env.Client, awsEnv.AMIProvider)
	linkedMachineCache = cache.New(time.Minute*10, time.Second*10)
	linkController := &link.Controller{
		Cache: linkedMachineCache,
	}
	garbageCollectController = garbagecollect.NewController(env.Client, cloudProvider, linkController)
})

var _ = AfterSuite(func() {
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	awsEnv.Reset()
})

var _ = Describe("MachineGarbageCollect", func() {
	var instance *ec2.Instance
	var providerID string

	BeforeEach(func() {
		instanceID := fake.InstanceID()
		providerID = fmt.Sprintf("aws:///test-zone-1a/%s", instanceID)
		nodeTemplate := test.AWSNodeTemplate(v1alpha1.AWSNodeTemplateSpec{})
		provisioner := test.Provisioner(coretest.ProvisionerOptions{
			ProviderRef: &v1alpha5.ProviderRef{
				APIVersion: v1alpha5.TestingGroup + "v1alpha1",
				Kind:       "NodeTemplate",
				Name:       nodeTemplate.Name,
			},
		})
		instance = &ec2.Instance{
			State: &ec2.InstanceState{
				Name: aws.String(ec2.InstanceStateNameRunning),
			},
			Tags: []*ec2.Tag{
				{
					Key:   aws.String(fmt.Sprintf("kubernetes.io/cluster/%s", settings.FromContext(ctx).ClusterName)),
					Value: aws.String("owned"),
				},
				{
					Key:   aws.String(v1alpha5.ProvisionerNameLabelKey),
					Value: aws.String(provisioner.Name),
				},
				{
					Key:   aws.String(v1alpha5.ManagedByLabelKey),
					Value: aws.String(settings.FromContext(ctx).ClusterName),
				},
			},
			PrivateDnsName: aws.String(fake.PrivateDNSName()),
			Placement: &ec2.Placement{
				AvailabilityZone: aws.String("test-zone-1a"),
			},
			InstanceId:   aws.String(instanceID),
			InstanceType: aws.String("m5.large"),
		}
	})
	AfterEach(func() {
		ExpectCleanedUp(ctx, env.Client)
		linkedMachineCache.Flush()
	})

	It("should delete an instance if there is no machine owner", func() {
		// Launch time was 10m ago
		instance.LaunchTime = aws.Time(time.Now().Add(-time.Minute * 10))
		awsEnv.EC2API.Instances.Store(aws.StringValue(instance.InstanceId), instance)

		ExpectReconcileSucceeded(ctx, garbageCollectController, client.ObjectKey{})
		_, err := cloudProvider.Get(ctx, providerID)
		Expect(err).To(HaveOccurred())
		Expect(corecloudprovider.IsMachineNotFoundError(err)).To(BeTrue())
	})
	It("should delete an instance along with the node if there is no machine owner (to quicken scheduling)", func() {
		// Launch time was 10m ago
		instance.LaunchTime = aws.Time(time.Now().Add(-time.Minute * 10))
		awsEnv.EC2API.Instances.Store(aws.StringValue(instance.InstanceId), instance)

		node := coretest.Node(coretest.NodeOptions{
			ProviderID: providerID,
		})
		ExpectApplied(ctx, env.Client, node)

		ExpectReconcileSucceeded(ctx, garbageCollectController, client.ObjectKey{})
		_, err := cloudProvider.Get(ctx, providerID)
		Expect(err).To(HaveOccurred())
		Expect(corecloudprovider.IsMachineNotFoundError(err)).To(BeTrue())

		ExpectNotFound(ctx, env.Client, node)
	})
	It("should delete many instances if they all don't have machine owners", func() {
		// Generate 500 instances that have different instanceIDs
		var ids []string
		for i := 0; i < 500; i++ {
			instanceID := fake.InstanceID()
			awsEnv.EC2API.Instances.Store(
				instanceID,
				&ec2.Instance{
					State: &ec2.InstanceState{
						Name: aws.String(ec2.InstanceStateNameRunning),
					},
					Tags: []*ec2.Tag{
						{
							Key:   aws.String(fmt.Sprintf("kubernetes.io/cluster/%s", settings.FromContext(ctx).ClusterName)),
							Value: aws.String("owned"),
						},
						{
							Key:   aws.String(v1alpha5.ProvisionerNameLabelKey),
							Value: aws.String("default"),
						},
						{
							Key:   aws.String(v1alpha5.ManagedByLabelKey),
							Value: aws.String(settings.FromContext(ctx).ClusterName),
						},
					},
					PrivateDnsName: aws.String(fake.PrivateDNSName()),
					Placement: &ec2.Placement{
						AvailabilityZone: aws.String("test-zone-1a"),
					},
					// Launch time was 10m ago
					LaunchTime:   aws.Time(time.Now().Add(-time.Minute * 10)),
					InstanceId:   aws.String(instanceID),
					InstanceType: aws.String("m5.large"),
				},
			)
			ids = append(ids, instanceID)
		}
		ExpectReconcileSucceeded(ctx, garbageCollectController, client.ObjectKey{})

		wg := sync.WaitGroup{}
		for _, id := range ids {
			wg.Add(1)
			go func(id string) {
				defer GinkgoRecover()
				defer wg.Done()

				_, err := cloudProvider.Get(ctx, fmt.Sprintf("aws:///test-zone-1a/%s", id))
				Expect(err).To(HaveOccurred())
				Expect(corecloudprovider.IsMachineNotFoundError(err)).To(BeTrue())
			}(id)
		}
		wg.Wait()
	})
	It("should not delete all instances if they all have machine owners", func() {
		// Generate 500 instances that have different instanceIDs
		var ids []string
		var machines []*v1alpha5.Machine
		for i := 0; i < 500; i++ {
			instanceID := fake.InstanceID()
			awsEnv.EC2API.Instances.Store(
				instanceID,
				&ec2.Instance{
					State: &ec2.InstanceState{
						Name: aws.String(ec2.InstanceStateNameRunning),
					},
					Tags: []*ec2.Tag{
						{
							Key:   aws.String(fmt.Sprintf("kubernetes.io/cluster/%s", settings.FromContext(ctx).ClusterName)),
							Value: aws.String("owned"),
						},
						{
							Key:   aws.String(v1alpha5.ProvisionerNameLabelKey),
							Value: aws.String("default"),
						},
						{
							Key:   aws.String(v1alpha5.ManagedByLabelKey),
							Value: aws.String(settings.FromContext(ctx).ClusterName),
						},
					},
					PrivateDnsName: aws.String(fake.PrivateDNSName()),
					Placement: &ec2.Placement{
						AvailabilityZone: aws.String("test-zone-1a"),
					},
					// Launch time was 10m ago
					LaunchTime:   aws.Time(time.Now().Add(-time.Minute * 10)),
					InstanceId:   aws.String(instanceID),
					InstanceType: aws.String("m5.large"),
				},
			)
			machine := coretest.Machine(v1alpha5.Machine{
				Status: v1alpha5.MachineStatus{
					ProviderID: fmt.Sprintf("aws:///test-zone-1a/%s", instanceID),
				},
			})
			ExpectApplied(ctx, env.Client, machine)
			machines = append(machines, machine)
			ids = append(ids, instanceID)
		}
		ExpectReconcileSucceeded(ctx, garbageCollectController, client.ObjectKey{})

		wg := sync.WaitGroup{}
		for _, id := range ids {
			wg.Add(1)
			go func(id string) {
				defer GinkgoRecover()
				defer wg.Done()

				_, err := cloudProvider.Get(ctx, fmt.Sprintf("aws:///test-zone-1a/%s", id))
				Expect(err).ToNot(HaveOccurred())
			}(id)
		}
		wg.Wait()

		for _, machine := range machines {
			ExpectExists(ctx, env.Client, machine)
		}
	})
	It("should not delete an instance if it is within the machine resolution window (1m)", func() {
		// Launch time just happened
		instance.LaunchTime = aws.Time(time.Now())
		awsEnv.EC2API.Instances.Store(aws.StringValue(instance.InstanceId), instance)

		ExpectReconcileSucceeded(ctx, garbageCollectController, client.ObjectKey{})
		_, err := cloudProvider.Get(ctx, providerID)
		Expect(err).NotTo(HaveOccurred())
	})
	It("should not delete an instance if it was not launched by a machine", func() {
		// Remove the "karpenter.sh/managed-by" tag (this isn't launched by a machine)
		instance.Tags = lo.Reject(instance.Tags, func(t *ec2.Tag, _ int) bool {
			return aws.StringValue(t.Key) == v1alpha5.ManagedByLabelKey
		})

		// Launch time was 10m ago
		instance.LaunchTime = aws.Time(time.Now().Add(-time.Minute * 10))
		awsEnv.EC2API.Instances.Store(aws.StringValue(instance.InstanceId), instance)

		ExpectReconcileSucceeded(ctx, garbageCollectController, client.ObjectKey{})
		_, err := cloudProvider.Get(ctx, providerID)
		Expect(err).NotTo(HaveOccurred())
	})
	It("should not delete the instance or node if it already has a machine that matches it", func() {
		// Launch time was 10m ago
		instance.LaunchTime = aws.Time(time.Now().Add(-time.Minute * 10))
		awsEnv.EC2API.Instances.Store(aws.StringValue(instance.InstanceId), instance)

		machine := coretest.Machine(v1alpha5.Machine{
			Status: v1alpha5.MachineStatus{
				ProviderID: providerID,
			},
		})
		node := coretest.Node(coretest.NodeOptions{
			ProviderID: providerID,
		})
		ExpectApplied(ctx, env.Client, machine, node)

		ExpectReconcileSucceeded(ctx, garbageCollectController, client.ObjectKey{})
		_, err := cloudProvider.Get(ctx, providerID)
		Expect(err).ToNot(HaveOccurred())
		ExpectExists(ctx, env.Client, node)
	})
	It("should not delete an instance if it is linked", func() {
		// Launch time was 10m ago
		instance.LaunchTime = aws.Time(time.Now().Add(-time.Minute * 10))
		awsEnv.EC2API.Instances.Store(aws.StringValue(instance.InstanceId), instance)

		// Create a machine that is actively linking
		machine := coretest.Machine(v1alpha5.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					v1alpha5.MachineLinkedAnnotationKey: providerID,
				},
			},
		})
		ExpectApplied(ctx, env.Client, machine)

		ExpectReconcileSucceeded(ctx, garbageCollectController, client.ObjectKey{})
		_, err := cloudProvider.Get(ctx, providerID)
		Expect(err).NotTo(HaveOccurred())
	})
	It("should not delete an instance if it is recently linked but the machine doesn't exist", func() {
		// Launch time was 10m ago
		instance.LaunchTime = aws.Time(time.Now().Add(-time.Minute * 10))
		awsEnv.EC2API.Instances.Store(aws.StringValue(instance.InstanceId), instance)

		// Add a provider id to the recently linked cache
		linkedMachineCache.SetDefault(providerID, nil)

		ExpectReconcileSucceeded(ctx, garbageCollectController, client.ObjectKey{})
		_, err := cloudProvider.Get(ctx, providerID)
		Expect(err).NotTo(HaveOccurred())
	})
})
