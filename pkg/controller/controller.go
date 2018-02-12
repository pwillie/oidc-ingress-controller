package controller

import (
	"crypto/md5"
	"fmt"
	"time"

	"github.com/golang/glog"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	extensions "k8s.io/api/extensions/v1beta1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	extensionlisters "k8s.io/client-go/listers/extensions/v1beta1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

const controllerAgentName = "oidc-ingress-controller"

const (
	classAnnotation           = "kubernetes.io/ingress.class"
	generatedAnnotation       = "pwillie/generated-by"
	originNamespaceAnnotation = "pwillie/origin-namespace"
	originNameAnnotation      = "pwillie/origin-name"
	SuccessSynced             = "Synced"
	ErrResourceExists         = "ErrResourceExists"
	MessageResourceExists     = "Resource %q already exists and is not managed by Foo"
	MessageResourceSynced     = "Foo synced successfully"
)

type Controller struct {
	kubeclientset        kubernetes.Interface
	ingressLister        extensionlisters.IngressLister
	ingressSynced        cache.InformerSynced
	workqueue            workqueue.RateLimitingInterface
	recorder             record.EventRecorder
	kubeInformerFactory  kubeinformers.SharedInformerFactory
	oidcClientAnnotation string
	oidcAuthNamespace    string
	oidcAuthServiceName  string
	oidcAuthServicePort  int
}

func NewController(
	kubeclientset kubernetes.Interface,
	oidcClientAnnotation string,
	oidcAuthNamespace string,
	oidcAuthServiceName string,
	oidcAuthServicePort int,
) *Controller {

	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeclientset, time.Second*30)

	ingressInformer := kubeInformerFactory.Extensions().V1beta1().Ingresses()
	glog.V(4).Info("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeclientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerAgentName})

	controller := &Controller{
		kubeclientset:        kubeclientset,
		ingressLister:        ingressInformer.Lister(),
		ingressSynced:        ingressInformer.Informer().HasSynced,
		workqueue:            workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Ingresses"),
		recorder:             recorder,
		kubeInformerFactory:  kubeInformerFactory,
		oidcClientAnnotation: oidcClientAnnotation,
		oidcAuthNamespace:    oidcAuthNamespace,
		oidcAuthServiceName:  oidcAuthServiceName,
		oidcAuthServicePort:  oidcAuthServicePort,
	}

	glog.Info("Setting up event handlers")
	ingressInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.handleObject,
		UpdateFunc: func(old, new interface{}) {
			newDepl := new.(*extensions.Ingress)
			oldDepl := old.(*extensions.Ingress)
			if newDepl.ResourceVersion == oldDepl.ResourceVersion {
				return
			}
			controller.handleObject(new)
		},
		DeleteFunc: controller.handleObject,
	})

	return controller
}

func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	glog.Info("Starting Ingress OIDC controller")

	glog.Info("Starting Informer Factory")
	go c.kubeInformerFactory.Start(stopCh)

	glog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.ingressSynced, c.ingressSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	// TODO: verify all auth ingress resources still valid??

	glog.Info("Starting workers")

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	glog.Info("Started workers")
	<-stopCh
	glog.Info("Shutting down workers")

	return nil
}

func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	err := func(obj interface{}) error {

		defer c.workqueue.Done(obj)
		var key string
		var ok bool

		if key, ok = obj.(string); !ok {
			c.workqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}

		if err := c.syncHandler(key); err != nil {
			return fmt.Errorf("error syncing '%s': %s", key, err.Error())
		}

		c.workqueue.Forget(obj)
		glog.Infof("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}

	return true
}

func (c *Controller) syncHandler(key string) error {

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	ingress, err := c.ingressLister.Ingresses(namespace).Get(name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("ingress '%s' in work queue no longer exists", key))
			// ensure we don't have any dangling ingress resources
			c.kubeclientset.ExtensionsV1beta1().Ingresses(c.oidcAuthNamespace).Delete(authIngressName(namespace, name), nil)
			return nil
		}
		return err
	}

	if _, ok := ingress.Annotations[c.oidcClientAnnotation]; ok {
		err = c.createAuthIngress(ingress)
		if err != nil {
			return err
		}
		return c.addAuthAnnotations(ingress)
	}
	// ensure we don't have any dangling ingress resources
	c.kubeclientset.ExtensionsV1beta1().Ingresses(c.oidcAuthNamespace).Delete(authIngressName(namespace, name), nil)
	if val, ok := ingress.Annotations[generatedAnnotation]; ok && val == controllerAgentName {
		c.deleteAuthAnnotations(ingress)
	}
	return nil
}

func (c *Controller) enqueueIngress(obj interface{}) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		runtime.HandleError(err)
		return
	}
	c.workqueue.AddRateLimited(key)
}

func (c *Controller) handleObject(obj interface{}) {
	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
		glog.V(4).Infof("Recovered deleted object '%s' from tombstone", object.GetName())
	}
	glog.V(4).Infof("Processing object: %s", object.GetName())
	c.enqueueIngress(object)
	return

}

func (c *Controller) makeAuthAnnotations(ingress *extensions.Ingress) map[string]string {
	// generate nginx auth annotations
	annotations := map[string]string{}
	annotations[generatedAnnotation] = controllerAgentName

	// found that different versions of nginx ingress controller are using different annotation names
	for _, domain := range []string{"ingress.kubernetes.io", "nginx.ingress.kubernetes.io"} {
		annotations[domain+"/auth-signin"] = "/auth/signin/" + ingress.Annotations[c.oidcClientAnnotation]
		annotations[domain+"/auth-url"] = fmt.Sprintf(
			"http://%s.%s.svc.cluster.local:%d/auth/verify/%s",
			c.oidcAuthServiceName,
			c.oidcAuthNamespace,
			c.oidcAuthServicePort,
			ingress.Annotations[c.oidcClientAnnotation],
		)
	}
	return annotations
}

func (c *Controller) addAuthAnnotations(ingress *extensions.Ingress) error {
	// add nginx auth annotations
	glog.Infof("Ingress " + ingress.Name + "/" + ingress.Namespace + " " + c.oidcClientAnnotation + ": " + ingress.Annotations[c.oidcClientAnnotation])
	ingressCopy := ingress.DeepCopy()
	for key, val := range c.makeAuthAnnotations(ingress) {
		ingressCopy.Annotations[key] = val
	}
	_, err := c.kubeclientset.ExtensionsV1beta1().Ingresses(ingress.Namespace).Update(ingressCopy)
	return err
}

func (c *Controller) deleteAuthAnnotations(ingress *extensions.Ingress) error {
	// remove nginx auth annotations
	glog.Infof("Ingress " + ingress.Name + "/" + ingress.Namespace + " " + c.oidcClientAnnotation + ": " + ingress.Annotations[c.oidcClientAnnotation])
	if val, ok := ingress.Annotations[generatedAnnotation]; ok && val != controllerAgentName {
		return nil
	}
	ingressCopy := ingress.DeepCopy()
	for key := range c.makeAuthAnnotations(ingress) {
		delete(ingressCopy.Annotations, key)
	}
	_, err := c.kubeclientset.ExtensionsV1beta1().Ingresses(ingress.Namespace).Update(ingressCopy)
	return err
}

func (c *Controller) createAuthIngress(ingress *extensions.Ingress) error {
	var authIngress *extensions.Ingress
	aname := authIngressName(ingress.GetNamespace(), ingress.GetName())
	ing, err := c.ingressLister.Ingresses(c.oidcAuthNamespace).Get(aname)

	createIngress := false
	if err != nil {
		if apierrors.IsNotFound(err) {
			createIngress = true
			authIngress = &extensions.Ingress{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: c.oidcAuthNamespace,
					Name:      aname,
				},
				Spec: extensions.IngressSpec{},
			}
			authIngress.Annotations = map[string]string{}
			authIngress.Annotations[generatedAnnotation] = controllerAgentName
			authIngress.Annotations[originNamespaceAnnotation] = ingress.GetNamespace()
			authIngress.Annotations[originNameAnnotation] = ingress.GetName()
			if class, ok := ingress.Annotations[classAnnotation]; ok {
				authIngress.Annotations[classAnnotation] = class
			}
		} else {
			return errors.Wrapf(err, "could not check for existing ingress %s/%s", c.oidcAuthNamespace, aname)
		}
	} else {
		authIngress = ing.DeepCopy()
	}

	// only modify ingress if we created it ie. has our annotation
	if authIngress.Annotations[generatedAnnotation] == controllerAgentName {
		authIngress.Spec.Rules = make([]extensions.IngressRule, 0, len(ingress.Spec.Rules))
		for _, rule := range ingress.Spec.Rules {
			glog.Infof("Ingress " + ingress.Name + "/" + ingress.Namespace + " Host: " + rule.Host)
			authIngress.Spec.Rules = append(authIngress.Spec.Rules, extensions.IngressRule{
				Host: rule.Host,
				IngressRuleValue: extensions.IngressRuleValue{
					HTTP: &extensions.HTTPIngressRuleValue{
						Paths: []extensions.HTTPIngressPath{
							{
								Path: "/auth",
								Backend: extensions.IngressBackend{
									ServiceName: c.oidcAuthServiceName,
									ServicePort: intstr.FromInt(c.oidcAuthServicePort),
								},
							},
						},
					},
				},
			})
		}
	}

	if createIngress {
		_, err = c.kubeclientset.ExtensionsV1beta1().Ingresses(c.oidcAuthNamespace).Create(authIngress)
		glog.Infof("Create ingress: %+v\n", err)
		return err
	}
	_, err = c.kubeclientset.ExtensionsV1beta1().Ingresses(c.oidcAuthNamespace).Update(authIngress)
	glog.Infof("Update ingress: %+v\n", err)
	return err
}

func authIngressName(namespace, name string) string {
	return fmt.Sprintf("%x", md5.Sum([]byte(namespace+"/"+name)))
}
