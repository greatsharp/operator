package pkg

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	corev1Informer "k8s.io/client-go/informers/core/v1"
	networkingInformer "k8s.io/client-go/informers/networking/v1"
	"k8s.io/client-go/kubernetes"
	corev1Lister "k8s.io/client-go/listers/core/v1"
	netowrkingLister "k8s.io/client-go/listers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"reflect"
	"time"
)

const workers = 5
const maxRetry = 10

type controller struct {
	client        kubernetes.Interface
	ingressLister netowrkingLister.IngressLister
	serviceLister corev1Lister.ServiceLister
	queue         workqueue.RateLimitingInterface
}

func (c *controller) deleteIngress(obj interface{}) {
	ingress := obj.(*networkingv1.Ingress)
	fmt.Println("delete ingress", ingress.Namespace+"/"+ingress.Name)
	ownerReference := metav1.GetControllerOf(ingress)
	if ownerReference == nil {
		return
	}
	if ownerReference.Kind != "Service" {
		return
	}
	c.queue.Add(ingress.Namespace + "/" + ingress.Name)
}

func (c *controller) addService(obj interface{}) {
	c.enqueue(obj)
}

func (c *controller) updateService(oldObj interface{}, newObj interface{}) {
	if reflect.DeepEqual(oldObj, newObj) {
		return
	}

	c.enqueue(newObj)
}

func (c *controller) enqueue(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
	}

	fmt.Println("enqueue service", key)
	c.queue.Add(key)
}

func (c *controller) Run(stopCh chan struct{}) {
	for i := 0; i < workers; i++ {
		go wait.Until(c.worker, time.Minute, stopCh)
	}

	<-stopCh
}

func (c *controller) worker() {
	for c.processNextItem() {

	}
}

func (c *controller) processNextItem() bool {
	item, shutdown := c.queue.Get()
	if shutdown {
		return false
	}
	defer c.queue.Done(item)

	key := item.(string)
	err := c.syncService(key)
	if err != nil {
		c.handleError(key, err)
	}

	return true
}

func (c *controller) syncService(key string) error {
	namespaceKey, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	// service已被删除
	service, err := c.serviceLister.Services(namespaceKey).Get(name)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	_, ok := service.GetAnnotations()["ingress/http"]
	ingress, err := c.ingressLister.Ingresses(namespaceKey).Get(name)
	if err != nil && !errors.IsNotFound(err) {
		return err
	}

	if ok && errors.IsNotFound(err) {
		// service上有ingress注解，但ingress不存在则需要创建ingress
		ig := c.constructIngress(service)
		_, err := c.client.NetworkingV1().Ingresses(namespaceKey).Create(context.TODO(), ig, metav1.CreateOptions{})
		if err != nil {
			return err
		}
		fmt.Println("create ingress", ig.Namespace+"/"+ig.Name)
	} else if !ok && ingress != nil {
		// service上没有ingress注解，但ingress存在，则必须删除对应的ingress
		err := c.client.NetworkingV1().Ingresses(namespaceKey).Delete(context.TODO(), name, metav1.DeleteOptions{})
		if err != nil {
			return err
		}
		fmt.Println("delete ingress", ingress.Namespace+"/"+ingress.Name)
	}

	return nil
}

func (c *controller) handleError(key string, err error) {
	if c.queue.NumRequeues(key) <= maxRetry {
		c.queue.AddRateLimited(key)
		return
	}

	runtime.HandleError(err)
	c.queue.Forget(key)
}

func (c *controller) constructIngress(service *corev1.Service) *networkingv1.Ingress {
	ingress := networkingv1.Ingress{}

	ownerReference := metav1.NewControllerRef(service, corev1.SchemeGroupVersion.WithKind("Service"))
	ingress.ObjectMeta.OwnerReferences = []metav1.OwnerReference{
		*ownerReference,
	}

	ingress.Name = service.Name
	ingress.Namespace = service.Namespace
	pathType := networkingv1.PathTypePrefix
	icn := "nginx"
	ingress.Spec = networkingv1.IngressSpec{
		IngressClassName: &icn,
		Rules: []networkingv1.IngressRule{{
			Host: "example.com",
			IngressRuleValue: networkingv1.IngressRuleValue{
				HTTP: &networkingv1.HTTPIngressRuleValue{
					Paths: []networkingv1.HTTPIngressPath{
						{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: service.Name,
									Port: networkingv1.ServiceBackendPort{
										Number: 80,
									},
								},
							},
						},
					},
				},
			},
		}},
	}

	return &ingress
}

func NewController(client kubernetes.Interface, serviceInformer corev1Informer.ServiceInformer, ingressInformer networkingInformer.IngressInformer) controller {
	c := controller{
		client:        client,
		ingressLister: ingressInformer.Lister(),
		serviceLister: serviceInformer.Lister(),
		queue:         workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
	}

	serviceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addService,
		UpdateFunc: c.updateService,
	})

	ingressInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: c.deleteIngress,
	})

	return c
}
