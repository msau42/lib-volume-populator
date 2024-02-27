/*
Copyright 2024 The Kubernetes Authors.

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

package populator_machinery

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/dynamic/dynamiclister"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	storagelisters "k8s.io/client-go/listers/storage/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/component-helpers/storage/volume"
	"k8s.io/klog/v2"
	gatewayclientset "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"
	gatewayInformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
	referenceGrantv1beta1 "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1beta1"
)

const (
	populatorContainerName      = "populate"
	populatorPodPrefix          = "populate"
	populatorStorageClassPrefix = "populate"
	populatorPodVolumeName      = "target"
	populatorPvcPrefix          = "prime"
	populatedFromAnnoSuffix     = "populated-from"
	pvcFinalizerSuffix          = "populate-target-protection"
	annSelectedNode             = "volume.kubernetes.io/selected-node"
	annStorageClassPrime        = "volume-populator/storage-class-prime"
	controllerNameSuffix        = "populator"

	reasonPodCreationError              = "PopulatorCreationError"
	reasonPodCreationSuccess            = "PopulatorCreated"
	reasonPodFailed                     = "PopulatorFailed"
	reasonPodFinished                   = "PopulatorFinished"
	reasonPopulateOperationStartError   = "PopulateOperationStartError"
	reasonPopulateOperationStartSuccess = "PopulateOperationStartSuccess"
	reasonPopulateOperationFailed       = "PopulateOperationFailed"
	reasonPopulateOperationFinished     = "PopulateOperationFinished"
	reasonPVCCreationError              = "PopulatorPVCCreationError"
)

type empty struct{}

type stringSet struct {
	set map[string]empty
}

type controller struct {
	populatorNamespace   string
	populatedFromAnno    string
	pvcFinalizer         string
	kubeClient           kubernetes.Interface
	imageName            string
	devicePath           string
	mountPath            string
	pvcLister            corelisters.PersistentVolumeClaimLister
	pvcSynced            cache.InformerSynced
	pvLister             corelisters.PersistentVolumeLister
	pvSynced             cache.InformerSynced
	podLister            corelisters.PodLister
	podSynced            cache.InformerSynced
	scLister             storagelisters.StorageClassLister
	scSynced             cache.InformerSynced
	unstLister           dynamiclister.Lister
	unstSynced           cache.InformerSynced
	mu                   sync.Mutex
	notifyMap            map[string]*stringSet
	cleanupMap           map[string]*stringSet
	workqueue            workqueue.RateLimitingInterface
	populatorArgs        func(bool, *unstructured.Unstructured) ([]string, error)
	gk                   schema.GroupKind
	metrics              *metricsManager
	recorder             record.EventRecorder
	referenceGrantLister referenceGrantv1beta1.ReferenceGrantLister
	referenceGrantSynced cache.InformerSynced
	useProviderImpl      bool
	populate             func(context.Context, *PopulatorParams) error
	populateComplete     func(context.Context, *PopulatorParams) (bool, error)
}

type VolumePopulatorConfig struct {
	MasterURL     string
	Kubeconfig    string
	ImageName     string
	HttpEndpoint  string
	MetricsPath   string
	Namespace     string
	Prefix        string
	Gk            schema.GroupKind
	Gvr           schema.GroupVersionResource
	MountPath     string
	DevicePath    string
	PopulatorArgs func(bool, *unstructured.Unstructured) ([]string, error)
	// Boolean flag which determines whether or not to use cloud provider specific data populate functions
	UseProviderImpl bool
	// Data population function, invoked when the useProviderImpl variable is set to true
	Populate func(context.Context, *PopulatorParams) error
	// Data population completeness check function, return true when data transfer gets completed.
	// Invoked when the useProviderImpl variable is set to true
	PopulateComplete func(context.Context, *PopulatorParams) (bool, error)
}

type PopulatorParams struct {
	KubeClient   kubernetes.Interface
	StorageClass *storagev1.StorageClass
	PvcPrime     *corev1.PersistentVolumeClaim
	Pvc          *corev1.PersistentVolumeClaim
	Unstructured *unstructured.Unstructured
}

func RunController(masterURL, kubeconfig, imageName, httpEndpoint, metricsPath, namespace, prefix string,
	gk schema.GroupKind, gvr schema.GroupVersionResource, mountPath, devicePath string,
	populatorArgs func(bool, *unstructured.Unstructured) ([]string, error),
) {
	vpcfg := &VolumePopulatorConfig{
		MasterURL:       masterURL,
		Kubeconfig:      kubeconfig,
		ImageName:       imageName,
		HttpEndpoint:    httpEndpoint,
		MetricsPath:     metricsPath,
		Namespace:       namespace,
		Prefix:          prefix,
		Gk:              gk,
		Gvr:             gvr,
		MountPath:       mountPath,
		DevicePath:      devicePath,
		PopulatorArgs:   populatorArgs,
		UseProviderImpl: false,
	}
	RunControllerWithConfig(vpcfg)
}

func RunControllerWithConfig(vpcfg *VolumePopulatorConfig) {
	klog.Infof("Starting populator controller for %s", vpcfg.Gk)

	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		close(stopCh)
		<-sigCh
		os.Exit(1) // second signal. Exit directly.
	}()

	cfg, err := clientcmd.BuildConfigFromFlags(vpcfg.MasterURL, vpcfg.Kubeconfig)
	if err != nil {
		klog.Fatalf("Failed to create config: %v", err)
	}

	kubeClient, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("Failed to create client: %v", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("Failed to create dynamic client: %v", err)
	}

	gatewayClient, err := gatewayclientset.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("Failed to create gateway client: %v", err)
	}

	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Second*30)
	dynInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, time.Second*30)

	pvcInformer := kubeInformerFactory.Core().V1().PersistentVolumeClaims()
	pvInformer := kubeInformerFactory.Core().V1().PersistentVolumes()
	podInformer := kubeInformerFactory.Core().V1().Pods()
	scInformer := kubeInformerFactory.Storage().V1().StorageClasses()
	unstInformer := dynInformerFactory.ForResource(vpcfg.Gvr).Informer()

	gatewayInformerFactory := gatewayInformers.NewSharedInformerFactory(gatewayClient, time.Second*30)
	referenceGrants := gatewayInformerFactory.Gateway().V1beta1().ReferenceGrants()

	c := &controller{
		kubeClient:           kubeClient,
		imageName:            vpcfg.ImageName,
		populatorNamespace:   vpcfg.Namespace,
		devicePath:           vpcfg.DevicePath,
		mountPath:            vpcfg.MountPath,
		populatedFromAnno:    vpcfg.Prefix + "/" + populatedFromAnnoSuffix,
		pvcFinalizer:         vpcfg.Prefix + "/" + pvcFinalizerSuffix,
		pvcLister:            pvcInformer.Lister(),
		pvcSynced:            pvcInformer.Informer().HasSynced,
		pvLister:             pvInformer.Lister(),
		pvSynced:             pvInformer.Informer().HasSynced,
		podLister:            podInformer.Lister(),
		podSynced:            podInformer.Informer().HasSynced,
		scLister:             scInformer.Lister(),
		scSynced:             scInformer.Informer().HasSynced,
		unstLister:           dynamiclister.New(unstInformer.GetIndexer(), vpcfg.Gvr),
		unstSynced:           unstInformer.HasSynced,
		notifyMap:            make(map[string]*stringSet),
		cleanupMap:           make(map[string]*stringSet),
		workqueue:            workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		populatorArgs:        vpcfg.PopulatorArgs,
		gk:                   vpcfg.Gk,
		metrics:              initMetrics(),
		recorder:             getRecorder(kubeClient, vpcfg.Prefix+"-"+controllerNameSuffix),
		referenceGrantLister: referenceGrants.Lister(),
		referenceGrantSynced: referenceGrants.Informer().HasSynced,
		useProviderImpl:      vpcfg.UseProviderImpl,
		populate:             vpcfg.Populate,
		populateComplete:     vpcfg.PopulateComplete,
	}

	c.metrics.startListener(vpcfg.HttpEndpoint, vpcfg.MetricsPath)
	defer c.metrics.stopListener()

	pvcInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handlePVC,
		UpdateFunc: func(old, new interface{}) {
			newPvc := new.(*corev1.PersistentVolumeClaim)
			oldPvc := old.(*corev1.PersistentVolumeClaim)
			if newPvc.ResourceVersion == oldPvc.ResourceVersion {
				return
			}
			c.handlePVC(new)
		},
		DeleteFunc: c.handlePVC,
	})

	pvInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handlePV,
		UpdateFunc: func(old, new interface{}) {
			newPv := new.(*corev1.PersistentVolume)
			oldPv := old.(*corev1.PersistentVolume)
			if newPv.ResourceVersion == oldPv.ResourceVersion {
				return
			}
			c.handlePV(new)
		},
		DeleteFunc: c.handlePV,
	})

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handlePod,
		UpdateFunc: func(old, new interface{}) {
			newPod := new.(*corev1.Pod)
			oldPod := old.(*corev1.Pod)
			if newPod.ResourceVersion == oldPod.ResourceVersion {
				return
			}
			c.handlePod(new)
		},
		DeleteFunc: c.handlePod,
	})

	scInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handleSC,
		UpdateFunc: func(old, new interface{}) {
			newSc := new.(*storagev1.StorageClass)
			oldSc := old.(*storagev1.StorageClass)
			if newSc.ResourceVersion == oldSc.ResourceVersion {
				return
			}
			c.handleSC(new)
		},
		DeleteFunc: c.handleSC,
	})

	unstInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.handleUnstructured,
		UpdateFunc: func(old, new interface{}) {
			newUnstructured := new.(*unstructured.Unstructured)
			oldUnstructured := old.(*unstructured.Unstructured)
			if newUnstructured.GetResourceVersion() == oldUnstructured.GetResourceVersion() {
				return
			}
			c.handleUnstructured(new)
		},
		DeleteFunc: c.handleUnstructured,
	})

	kubeInformerFactory.Start(stopCh)
	dynInformerFactory.Start(stopCh)
	gatewayInformerFactory.Start(stopCh)

	if err = c.run(stopCh); err != nil {
		klog.Fatalf("Failed to run controller: %v", err)
	}
}

func getRecorder(kubeClient kubernetes.Interface, controllerName string) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartStructuredLogging(0)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: controllerName})
	return recorder
}

func (c *controller) addNotification(keyToCall, objType, namespace, name string) {
	var key string
	if 0 == len(namespace) {
		key = objType + "/" + name
	} else {
		key = objType + "/" + namespace + "/" + name
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.notifyMap[key]
	if s == nil {
		s = &stringSet{make(map[string]empty)}
		c.notifyMap[key] = s
	}
	s.set[keyToCall] = empty{}
	s = c.cleanupMap[keyToCall]
	if s == nil {
		s = &stringSet{make(map[string]empty)}
		c.cleanupMap[keyToCall] = s
	}
	s.set[key] = empty{}
}

func (c *controller) cleanupNotifications(keyToCall string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.cleanupMap[keyToCall]
	if s == nil {
		return
	}
	for key := range s.set {
		t := c.notifyMap[key]
		if t == nil {
			continue
		}
		delete(t.set, keyToCall)
		if 0 == len(t.set) {
			delete(c.notifyMap, key)
		}
	}
}

func translateObject(obj interface{}) metav1.Object {
	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return nil
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return nil
		}
	}
	return object
}

func (c *controller) handleMapped(obj interface{}, objType string) {
	object := translateObject(obj)
	if object == nil {
		return
	}
	var key string
	if len(object.GetNamespace()) == 0 {
		key = objType + "/" + object.GetName()
	} else {
		key = objType + "/" + object.GetNamespace() + "/" + object.GetName()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.notifyMap[key]; ok {
		for k := range s.set {
			c.workqueue.Add(k)
		}
	}
}

func (c *controller) handlePVC(obj interface{}) {
	c.handleMapped(obj, "pvc")
	object := translateObject(obj)
	if object == nil {
		return
	}
	if c.populatorNamespace != object.GetNamespace() {
		c.workqueue.Add("pvc/" + object.GetNamespace() + "/" + object.GetName())
	}
}

func (c *controller) handlePV(obj interface{}) {
	c.handleMapped(obj, "pv")
}

func (c *controller) handlePod(obj interface{}) {
	c.handleMapped(obj, "pod")
}

func (c *controller) handleSC(obj interface{}) {
	c.handleMapped(obj, "sc")
}

func (c *controller) handleUnstructured(obj interface{}) {
	c.handleMapped(obj, "unstructured")
}

func (c *controller) run(stopCh <-chan struct{}) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	ok := cache.WaitForCacheSync(stopCh, c.pvcSynced, c.pvSynced, c.podSynced, c.scSynced, c.unstSynced, c.referenceGrantSynced)
	if !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh

	return nil
}

func (c *controller) runWorker() {
	processNextWorkItem := func(obj interface{}) error {
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		var err error
		parts := strings.Split(key, "/")
		switch parts[0] {
		case "pvc":
			if len(parts) != 3 {
				utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
				return nil
			}
			err = c.syncPvc(context.TODO(), key, parts[1], parts[2])
		default:
			utilruntime.HandleError(fmt.Errorf("invalid resource key: %s", key))
			return nil
		}
		if err != nil {
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		c.workqueue.Forget(obj)
		return nil
	}

	for {
		obj, shutdown := c.workqueue.Get()
		if shutdown {
			return
		}
		err := processNextWorkItem(obj)
		if err != nil {
			utilruntime.HandleError(err)
		}
	}
}

func (c *controller) syncPvc(ctx context.Context, key, pvcNamespace, pvcName string) error {
	if c.populatorNamespace == pvcNamespace {
		// Ignore PVCs in our own working namespace
		return nil
	}

	var err error

	var pvc *corev1.PersistentVolumeClaim
	pvc, err = c.pvcLister.PersistentVolumeClaims(pvcNamespace).Get(pvcName)
	if err != nil {
		if errors.IsNotFound(err) {
			utilruntime.HandleError(fmt.Errorf("pvc '%s' in work queue no longer exists", key))
			return nil
		}
		return err
	}

	dataSourceRef := pvc.Spec.DataSourceRef
	if dataSourceRef == nil {
		// Ignore PVCs without a datasource
		return nil
	}

	apiGroup := ""
	if dataSourceRef.APIGroup != nil {
		apiGroup = *dataSourceRef.APIGroup
	}
	if c.gk.Group != apiGroup || c.gk.Kind != dataSourceRef.Kind || "" == dataSourceRef.Name {
		// Ignore PVCs that aren't for this populator to handle
		return nil
	}

	dataSourceRefNamespace := pvc.Namespace
	if dataSourceRef.Namespace != nil && pvc.Namespace != *dataSourceRef.Namespace {
		dataSourceRefNamespace = *dataSourceRef.Namespace
		// Get all ReferenceGrants in data source's namespace
		referenceGrants, err := c.referenceGrantLister.ReferenceGrants(*dataSourceRef.Namespace).List(labels.Everything())
		if err != nil {
			return fmt.Errorf("error getting ReferenceGrants in %s namespace from api server: %v", *dataSourceRef.Namespace, err)
		}
		if allowed, err := IsGranted(ctx, pvc, referenceGrants); err != nil || !allowed {
			return err
		}
	}

	var unstructured *unstructured.Unstructured
	unstructured, err = c.unstLister.Namespace(dataSourceRefNamespace).Get(dataSourceRef.Name)
	if err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
		c.addNotification(key, "unstructured", pvc.Namespace, dataSourceRef.Name)
		// We'll get called again later when the data source exists
		return nil
	}

	var waitForFirstConsumer bool
	var nodeName string
	var storageClass *storagev1.StorageClass
	if pvc.Spec.StorageClassName != nil {
		storageClassName := *pvc.Spec.StorageClassName

		storageClass, err = c.scLister.Get(storageClassName)
		if err != nil {
			if !errors.IsNotFound(err) {
				return err
			}
			c.addNotification(key, "sc", "", storageClassName)
			// We'll get called again later when the storage class exists
			return nil
		}

		if err := c.checkIntreeStorageClass(pvc, storageClass); err != nil {
			klog.V(2).Infof("Ignoring PVC %s/%s: %s", pvcNamespace, pvcName, err)
			return nil
		}

		if storageClass.VolumeBindingMode != nil && storagev1.VolumeBindingWaitForFirstConsumer == *storageClass.VolumeBindingMode {
			waitForFirstConsumer = true
			nodeName = pvc.Annotations[annSelectedNode]
			if nodeName == "" {
				// Wait for the PVC to get a node name before continuing
				return nil
			}
		}
	}

	// Look for the populator pod
	var pod *corev1.Pod
	podName := fmt.Sprintf("%s-%s", populatorPodPrefix, pvc.UID)
	if !c.useProviderImpl {
		c.addNotification(key, "pod", c.populatorNamespace, podName)
		pod, err = c.podLister.Pods(c.populatorNamespace).Get(podName)
		if err != nil {
			if !errors.IsNotFound(err) {
				return err
			}
		}
	}

	// TODO: Clean up StroageClass prime when the original StorageClass gets deleted
	// TODO: Add finializer to ensure user doesn't delete the SCPrime
	// If populate data with cloud provider implementation, and orginal StorageClass's VolumeBindingMode is VolumeBindingWaitForFirstConsumer,
	// create a StorageClass with VolumeBindingImmediate for pvcPrime. Otherwise, pv will not get provisioned since we will not create the
	// populator pod
	storageClassPrimeName := *pvc.Spec.StorageClassName
	if c.useProviderImpl && storageClass.VolumeBindingMode != nil && storagev1.VolumeBindingWaitForFirstConsumer == *storageClass.VolumeBindingMode {
		storageClassPrimeName = fmt.Sprintf("%s-%s", populatorStorageClassPrefix, *pvc.Spec.StorageClassName)
		scPrime, err := c.scLister.Get(storageClassPrimeName)
		if err != nil {
			if !errors.IsNotFound(err) {
				return err
			}
			bm := storagev1.VolumeBindingImmediate
			scPrime = &storagev1.StorageClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:        storageClassPrimeName,
					Annotations: map[string]string{},
				},
				Provisioner:          storageClass.Provisioner,
				Parameters:           storageClass.Parameters,
				ReclaimPolicy:        storageClass.ReclaimPolicy,
				MountOptions:         storageClass.MountOptions,
				AllowVolumeExpansion: storageClass.AllowVolumeExpansion,
				VolumeBindingMode:    &bm,
				AllowedTopologies:    storageClass.AllowedTopologies,
			}
			if storageClass.Parameters != nil && storageClass.Parameters["volumeBindingMode"] != "" {
				scPrime.Parameters["volumeBindingMode"] = string(bm)
			}
			scPrime, err = c.kubeClient.StorageV1().StorageClasses().Create(ctx, scPrime, metav1.CreateOptions{})
			if err != nil {
				c.addNotification(key, "sc", "", storageClassPrimeName)
				return err
			}
		}
	}

	// Look for PVC'
	pvcPrimeName := fmt.Sprintf("%s-%s", populatorPvcPrefix, pvc.UID)
	c.addNotification(key, "pvc", c.populatorNamespace, pvcPrimeName)
	var pvcPrime *corev1.PersistentVolumeClaim
	pvcPrime, err = c.pvcLister.PersistentVolumeClaims(c.populatorNamespace).Get(pvcPrimeName)
	if err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	}

	// *** Here is the first place we start to create/modify objects ***
	// If the PVC is unbound and PVC' doesn't exist yet, create PVC'
	if "" == pvc.Spec.VolumeName && pvcPrime == nil {
		pvcPrime = &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcPrimeName,
				Namespace: c.populatorNamespace,
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      pvc.Spec.AccessModes,
				Resources:        pvc.Spec.Resources,
				StorageClassName: &storageClassPrimeName,
				VolumeMode:       pvc.Spec.VolumeMode,
			},
		}
		if waitForFirstConsumer {
			pvcPrime.Annotations = map[string]string{
				annSelectedNode: nodeName,
			}
		}
		pvcPrime, err = c.kubeClient.CoreV1().PersistentVolumeClaims(c.populatorNamespace).Create(ctx, pvcPrime, metav1.CreateOptions{})
		if err != nil {
			c.recorder.Eventf(pvc, corev1.EventTypeWarning, reasonPVCCreationError, "Failed to create populator PVC: %s", err)
			return err
		}
	}

	// If the PVC is unbound, we need to perform the population
	params := &PopulatorParams{
		KubeClient:   c.kubeClient,
		StorageClass: storageClass,
		Pvc:          pvc,
		PvcPrime:     pvcPrime,
		Unstructured: unstructured,
	}
	if "" == pvc.Spec.VolumeName {

		// Ensure the PVC has a finalizer on it so we can clean up the stuff we create
		err = c.ensureFinalizer(ctx, pvc, c.pvcFinalizer, true)
		if err != nil {
			return err
		}

		// Record start time for populator metric
		c.metrics.operationStart(pvc.UID)

		// If use cloud provider specific implementation, invoke the populate() function. Otherwise, proceed with pod creation.
		if c.useProviderImpl {
			if "" == pvcPrime.Spec.VolumeName {
				// We'll get called again later when the pvc prime gets bounded
				return nil
			}
			err := c.populate(ctx, params)
			if err != nil {
				c.recorder.Eventf(pvc, corev1.EventTypeWarning, reasonPopulateOperationStartError, "Failed to start populate operation: %s", err)
				return err
			}
		} else {
			// If the pod doesn't exist yet, create it
			if pod == nil {
				var rawBlock bool
				if nil != pvc.Spec.VolumeMode && corev1.PersistentVolumeBlock == *pvc.Spec.VolumeMode {
					rawBlock = true
				}

				// Calculate the args for the populator pod
				var args []string
				args, err = c.populatorArgs(rawBlock, unstructured)
				if err != nil {
					return err
				}

				// Make the pod
				pod = &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      podName,
						Namespace: c.populatorNamespace,
					},
					Spec: makePopulatePodSpec(pvcPrimeName),
				}
				pod.Spec.Volumes[0].VolumeSource.PersistentVolumeClaim.ClaimName = pvcPrimeName
				con := &pod.Spec.Containers[0]
				con.Image = c.imageName
				con.Args = args
				if rawBlock {
					con.VolumeDevices = []corev1.VolumeDevice{
						{
							Name:       populatorPodVolumeName,
							DevicePath: c.devicePath,
						},
					}
				} else {
					con.VolumeMounts = []corev1.VolumeMount{
						{
							Name:      populatorPodVolumeName,
							MountPath: c.mountPath,
						},
					}
				}
				if waitForFirstConsumer {
					pod.Spec.NodeName = nodeName
				}
				_, err = c.kubeClient.CoreV1().Pods(c.populatorNamespace).Create(ctx, pod, metav1.CreateOptions{})
				if err != nil {
					c.recorder.Eventf(pvc, corev1.EventTypeWarning, reasonPodCreationError, "Failed to create populator pod: %s", err)
					return err
				}
				c.recorder.Eventf(pvc, corev1.EventTypeNormal, reasonPodCreationSuccess, "Populator started")

				// We'll get called again later when the pod exists
				return nil
			}
		}

		if c.useProviderImpl {
			complete, err := c.populateComplete(ctx, params)
			if err != nil {
				return err
			}
			if !complete {
				// We'll get called again later when the population operation complete
				return nil
			}
		} else {
			if corev1.PodSucceeded != pod.Status.Phase {
				if corev1.PodFailed == pod.Status.Phase {
					c.recorder.Eventf(pvc, corev1.EventTypeWarning, reasonPodFailed, "Populator failed: %s", pod.Status.Message)
					// Delete failed pods so we can try again
					err = c.kubeClient.CoreV1().Pods(c.populatorNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
					if err != nil {
						return err
					}
				}
				// We'll get called again later when the pod succeeds
				return nil
			}
		}

		// This would be bad
		if pvcPrime == nil {
			return fmt.Errorf("Failed to find PVC for populator pod")
		}

		// Get PV
		var pv *corev1.PersistentVolume
		c.addNotification(key, "pv", "", pvcPrime.Spec.VolumeName)
		pv, err = c.kubeClient.CoreV1().PersistentVolumes().Get(ctx, pvcPrime.Spec.VolumeName, metav1.GetOptions{})
		if err != nil {
			if !errors.IsNotFound(err) {
				return err
			}
			// We'll get called again later when the PV exists
			return nil
		}

		// Examine the claimref for the PV and see if it's bound to the correct PVC
		claimRef := pv.Spec.ClaimRef
		if claimRef.Name != pvc.Name || claimRef.Namespace != pvc.Namespace || claimRef.UID != pvc.UID {
			// Make new PV with strategic patch values to perform the PV rebind
			patchPv := corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{
					Name:        pv.Name,
					Annotations: map[string]string{},
				},
				Spec: corev1.PersistentVolumeSpec{
					ClaimRef: &corev1.ObjectReference{
						Namespace:       pvc.Namespace,
						Name:            pvc.Name,
						UID:             pvc.UID,
						ResourceVersion: pvc.ResourceVersion,
					},
				},
			}
			patchPv.Annotations[c.populatedFromAnno] = pvc.Namespace + "/" + dataSourceRef.Name
			var patchData []byte
			patchData, err = json.Marshal(patchPv)
			if err != nil {
				return err
			}
			_, err = c.kubeClient.CoreV1().PersistentVolumes().Patch(ctx, pv.Name, types.StrategicMergePatchType,
				patchData, metav1.PatchOptions{})
			if err != nil {
				return err
			}

			// Don't start cleaning up yet -- we need to bind controller to acknowledge
			// the switch
			return nil
		}
	}

	// Wait for the bind controller to rebind the PV
	if pvcPrime != nil {
		if corev1.ClaimLost != pvcPrime.Status.Phase {
			return nil
		}
	}

	// Record start time for populator metric
	c.metrics.recordMetrics(pvc.UID, "success")

	// *** At this point the volume population is done and we're just cleaning up ***
	c.recorder.Eventf(pvc, corev1.EventTypeNormal, reasonPodFinished, "Populator finished")

	if !c.useProviderImpl {
		// If the pod still exists, delete it
		if pod != nil {
			err = c.kubeClient.CoreV1().Pods(c.populatorNamespace).Delete(ctx, pod.Name, metav1.DeleteOptions{})
			if err != nil {
				return err
			}
		}
	}

	// If PVC' still exists, delete it
	if pvcPrime != nil {
		err = c.kubeClient.CoreV1().PersistentVolumeClaims(c.populatorNamespace).Delete(ctx, pvcPrime.Name, metav1.DeleteOptions{})
		if err != nil {
			return err
		}
	}

	// Make sure the PVC finalizer is gone
	err = c.ensureFinalizer(ctx, pvc, c.pvcFinalizer, false)
	if err != nil {
		return err
	}

	// Clean up our internal callback maps
	c.cleanupNotifications(key)

	return nil
}

func makePopulatePodSpec(pvcPrimeName string) corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name:            populatorContainerName,
				ImagePullPolicy: corev1.PullIfNotPresent,
			},
		},
		RestartPolicy: corev1.RestartPolicyNever,
		Volumes: []corev1.Volume{
			{
				Name: populatorPodVolumeName,
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: pvcPrimeName,
					},
				},
			},
		},
	}
}

func (c *controller) ensureFinalizer(ctx context.Context, pvc *corev1.PersistentVolumeClaim, finalizer string, want bool) error {
	finalizers := pvc.GetFinalizers()
	found := false
	foundIdx := -1
	for i, v := range finalizers {
		if finalizer == v {
			found = true
			foundIdx = i
			break
		}
	}
	if found == want {
		// Nothing to do in this case
		return nil
	}

	type patchOp struct {
		Op    string      `json:"op"`
		Path  string      `json:"path"`
		Value interface{} `json:"value,omitempty"`
	}

	var patch []patchOp

	if want {
		// Add the finalizer to the end of the list
		patch = []patchOp{
			{
				Op:    "test",
				Path:  "/metadata/finalizers",
				Value: finalizers,
			},
			{
				Op:    "add",
				Path:  "/metadata/finalizers/-",
				Value: finalizer,
			},
		}
	} else {
		// Remove the finalizer from the list index where it was found
		path := fmt.Sprintf("/metadata/finalizers/%d", foundIdx)
		patch = []patchOp{
			{
				Op:    "test",
				Path:  path,
				Value: finalizer,
			},
			{
				Op:   "remove",
				Path: path,
			},
		}
	}

	data, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = c.kubeClient.CoreV1().PersistentVolumeClaims(pvc.Namespace).Patch(ctx, pvc.Name, types.JSONPatchType,
		data, metav1.PatchOptions{})
	if err != nil {
		return err
	}

	return nil
}

func (c *controller) checkIntreeStorageClass(pvc *corev1.PersistentVolumeClaim, sc *storagev1.StorageClass) error {
	if !strings.HasPrefix(sc.Provisioner, "kubernetes.io/") {
		// This is not an in-tree StorageClass
		return nil
	}

	if pvc.Annotations != nil {
		if migrated := pvc.Annotations[volume.AnnMigratedTo]; migrated != "" {
			// The PVC is migrated to CSI
			return nil
		}
	}

	// The SC is in-tree & PVC is not migrated
	return fmt.Errorf("in-tree volume volume plugin %q cannot use volume populator", sc.Provisioner)
}
