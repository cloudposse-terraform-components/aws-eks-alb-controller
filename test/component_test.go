package test

import (
	"context"
	"testing"
	"strings"
	"fmt"
	"time"
	helper "github.com/cloudposse/test-helpers/pkg/atmos/component-helper"
	awsHelper "github.com/cloudposse/test-helpers/pkg/aws"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/cloudposse/test-helpers/pkg/atmos"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	networkingv1 "k8s.io/api/networking/v1"


	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

type ComponentSuite struct {
	helper.TestSuite
}

func (s *ComponentSuite) TestBasic() {
	const component = "eks/alb-controller/basic"
	const stack = "default-test"
	const awsRegion = "us-east-2"

	randomID := strings.ToLower(random.UniqueId())

	controllerNamespace := fmt.Sprintf("alb-controller-%s", randomID)

	input := map[string]interface{}{
		"kubernetes_namespace": controllerNamespace,
	}

	defer s.DestroyAtmosComponent(s.T(), component, stack, &input)
	options, _ := s.DeployAtmosComponent(s.T(), component, stack, &input)
	assert.NotNil(s.T(), options)

	type Metadata struct {
		AppVersion        string `json:"app_version"`
		Chart             string `json:"chart"`
		FirstDeployed     float64 `json:"first_deployed"`
		LastDeployed      float64 `json:"last_deployed"`
		Name              string `json:"name"`
		Namespace         string `json:"namespace"`
		Notes             string `json:"notes"`
		Revision          int    `json:"revision"`
		Values            string `json:"values"`
		Version           string `json:"version"`
	}

	metadataArray := []Metadata{}

	atmos.OutputStruct(s.T(), options, "metadata", &metadataArray)

	metadata := metadataArray[0]

	assert.Equal(s.T(), metadata.AppVersion, "v2.7.1")
	assert.Equal(s.T(), metadata.Chart, "aws-load-balancer-controller")
	assert.NotNil(s.T(), metadata.FirstDeployed)
	assert.NotNil(s.T(), metadata.LastDeployed)
	assert.Equal(s.T(), metadata.Name, "aws-load-balancer-controller")
	assert.Equal(s.T(), metadata.Namespace, controllerNamespace)
	assert.Equal(s.T(), metadata.Notes, "AWS Load Balancer controller installed!\n")
	assert.Equal(s.T(), metadata.Revision, 1)
	assert.NotNil(s.T(), metadata.Values)
	assert.Equal(s.T(), metadata.Version, "1.7.1")

	clusterOptions := s.GetAtmosOptions("eks/cluster", stack, nil)
	clusrerId := atmos.Output(s.T(), clusterOptions, "eks_cluster_id")
	cluster := awsHelper.GetEksCluster(s.T(), context.Background(), awsRegion, clusrerId)
	clientset, err := awsHelper.NewK8SClientset(cluster)
	assert.NoError(s.T(), err)
	assert.NotNil(s.T(), clientset)

	deployment, err := clientset.AppsV1().Deployments(controllerNamespace).Get(context.Background(), "aws-load-balancer-controller", metav1.GetOptions{})
	assert.NoError(s.T(), err)
	assert.NotNil(s.T(), deployment)
	assert.Equal(s.T(), deployment.Name, "aws-load-balancer-controller")
	assert.Equal(s.T(), deployment.Namespace, controllerNamespace)
	assert.Equal(s.T(), *deployment.Spec.Replicas, int32(2))


	ingressNamespace := fmt.Sprintf("example-%s", randomID)
	ingressName := fmt.Sprintf("example-ingress-%s", randomID)

	defer clientset.CoreV1().Namespaces().Delete(context.Background(), ingressNamespace, metav1.DeleteOptions{})
	_, err := clientset.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ingressNamespace,
		},
	}, metav1.CreateOptions{})
	assert.NoError(s.T(), err)


	pathType := networkingv1.PathTypeImplementationSpecific

	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ingressName,
			Annotations: map[string]string{
				"alb.ingress.kubernetes.io/load-balancer-name": ingressName,
				"alb.ingress.kubernetes.io/group.name":         "example-group",
				"external-dns.alpha.kubernetes.io/hostname":    "example.com",
			},
		},
		Spec: networkingv1.IngressSpec{
			DefaultBackend: &networkingv1.IngressBackend{
				Service: &networkingv1.IngressServiceBackend{
					Name: "default",
					Port: networkingv1.ServiceBackendPort{
						Name: "use-annotation",
					},
				},
			},
			TLS: []networkingv1.IngressTLS{
				{
					Hosts: []string{"example.com"},
				},
			},
			Rules: []networkingv1.IngressRule{
				{
					Host: "example.com",
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path: "/",
									PathType: &pathType,
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: "default",
											Port: networkingv1.ServiceBackendPort{
												Number: 80,
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	defer clientset.NetworkingV1().Ingresses(ingressNamespace).Delete(context.Background(), ingressName, metav1.DeleteOptions{})
	_, err = clientset.NetworkingV1().Ingresses(ingressNamespace).Create(context.Background(), ingress, metav1.CreateOptions{})
	assert.NoError(s.T(), err)
	assert.NotNil(s.T(), ingress)

	// Wait for the Ingress to be updated with the LoadBalancer metadata
	factory := informers.NewSharedInformerFactory(clientset, 0)
	informer := factory.Networking().V1().Ingresses().Informer()

	stopChannel := make(chan struct{})

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
            _, ok := oldObj.(*networkingv1.Ingress)
            if !ok {
                return
            }
            newIngress, ok := newObj.(*networkingv1.Ingress)
            if !ok {
                return
            }

            // Check if the Ingress's LoadBalancer status has been populated
            if len(newIngress.Status.LoadBalancer.Ingress[0].Hostname) > 0 {
                fmt.Printf("Ingress %s is ready\n", newIngress.Name)
				close(stopChannel)
            } else {
                fmt.Printf("Ingress %s is not ready yet\n", newIngress.Name)
            }
        },
	})

	go informer.Run(stopChannel)

	select {
		case <-stopChannel:
			msg := "All ingress have joined the EKS cluster"
			fmt.Println(msg)
		case <-time.After(1 * time.Minute):
			msg := "Not all worker nodes have joined the EKS cluster"
			fmt.Println(msg)
	}

	ingressStatus, err := clientset.NetworkingV1().Ingresses(ingressNamespace).Get(context.Background(), ingressName, metav1.GetOptions{})
	assert.NoError(s.T(), err)
	assert.Equal(s.T(), ingressStatus.Name, ingressName)
	assert.NotEmpty(s.T(), ingressStatus.Status.LoadBalancer.Ingress[0].Hostname)

	s.DriftTest(component, stack, nil)
}

func (s *ComponentSuite) TestEnabledFlag() {
	const component = "eks/alb-controller/disabled"
	const stack = "default-test"
	s.VerifyEnabledFlag(component, stack, nil)
}

func (s *ComponentSuite) SetupSuite() {
	s.TestSuite.InitConfig()
	s.TestSuite.Config.ComponentDestDir = "components/terraform/eks/alb-controller"
	s.TestSuite.SetupSuite()
}

func TestRunSuite(t *testing.T) {
	suite := new(ComponentSuite)
	suite.AddDependency(t, "vpc", "default-test", nil)
	suite.AddDependency(t, "eks/cluster", "default-test", nil)
	helper.Run(t, suite)
}
