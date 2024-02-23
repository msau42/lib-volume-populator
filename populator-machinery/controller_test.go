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
	"errors"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/dynamic/dynamiclister"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubeinformers "k8s.io/client-go/informers"
	informercorev1 "k8s.io/client-go/informers/core/v1"
	informerstoragev1 "k8s.io/client-go/informers/storage/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	gatewayfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
	gatewayInformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
)

type testCase struct {
	// Name of the test
	name string
	// Key added to the notifyMap
	key          string
	pvcNamespace string
	// PVC to be processed
	pvcName string

	// Object to insert into fake kubeclient/dynClient/gatewayClient before the test starts
	initialObjects []runtime.Object
	// Boolean flag which determines whether or not to use cloud provider specific data populate functions
	useProviderImpl bool
	// Data population function, invoked when the useProviderImpl variable is set to true
	populate func(context.Context, *PopulatorParams) error
	// Data population completeness check function, invoked when the useProviderImpl variable is set to true
	populateComplete func(context.Context, *PopulatorParams) (bool, error)
	// Expected errors
	expectedResult error
	// Expected keys in the notifyMap
	expectedKeys []string
	// Expected objects after the test runs
	expectedObjects []runtime.Object
}

const (
	testPrefix                         = "volume.populator.test"
	testVpWorkingNamespace             = "test"
	testPvcNamespace                   = "default"
	testPvcName                        = "test-pvc"
	testPvcUid                         = "test-uid"
	testApiGroup                       = "test.api.group"
	testApiVersion                     = testApiGroup + "/v1alpha1"
	testDatasourceKind                 = "TestKind"
	testDataSourceName                 = "test-data-source-name"
	testStorageClassName               = "test-sc"
	testStorageClassPrimeName          = populatorStorageClassPrefix + "-" + testStorageClassName
	testPopulatorPvcName               = populatorPvcPrefix + "-" + testPvcUid
	testPvName                         = "test-pv"
	testNodeName                       = "test-node-name"
	testPodName                        = populatorPodPrefix + "-" + testPvcUid
	testProvisioner                    = "test.provisioner"
	testPopulationOperationStartFailed = "Test populate operation start failed"
	testPopulateCompleteFailed         = "Test populate operation complete failed"
	dataSourceKey                      = "unstructured/" + testPvcNamespace + "/" + testDataSourceName
	storageClassKey                    = "sc/" + testStorageClassName
	podKey                             = "pod/" + testVpWorkingNamespace + "/" + testPodName
	scPrimeKey                         = "sc/" + testStorageClassPrimeName
	pvcPrimeKey                        = "pvc/" + testVpWorkingNamespace + "/" + testPopulatorPvcName
	pvKey                              = "pv/" + testPvName
)

func pvc(name, namespace, nodeName, scName, volumeName string, datasourceRef *v1.TypedObjectReference, phase v1.PersistentVolumeClaimPhase) *v1.PersistentVolumeClaim {
	return &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       testPvcUid,
			Annotations: map[string]string{
				annSelectedNode: nodeName,
			},
			Finalizers: []string{"kubernetes.io/pvc-protection"},
		},
		Spec: v1.PersistentVolumeClaimSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{
				v1.ReadWriteOnce,
			},
			StorageClassName: &scName,
			VolumeName:       volumeName,
			DataSourceRef:    datasourceRef,
		},
		Status: v1.PersistentVolumeClaimStatus{
			Phase: phase,
		},
	}
}

func dsf(apiGp, kind, name, namespace string) *v1.TypedObjectReference {
	return &v1.TypedObjectReference{
		APIGroup:  &apiGp,
		Kind:      kind,
		Name:      name,
		Namespace: &namespace,
	}
}

func ust() *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": testApiVersion,
			"kind":       testDatasourceKind,
			"metadata": map[string]any{
				"name":      testDataSourceName,
				"namespace": testPvcNamespace,
			},
		},
	}
}

func sc(scName string, volumeBindMode storagev1.VolumeBindingMode) *storagev1.StorageClass {
	p := testProvisioner
	return &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: scName,
		},
		Provisioner:       p,
		VolumeBindingMode: &volumeBindMode,
	}
}

func pod(phase corev1.PodPhase) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPodName,
			Namespace: testVpWorkingNamespace,
		},
		Status: v1.PodStatus{
			Phase: phase,
		},
	}
}

func pv(pvcName, pvcNamespace, pvcUid string) *v1.PersistentVolume {
	return &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: testPvName,
		},
		Spec: corev1.PersistentVolumeSpec{
			ClaimRef: &corev1.ObjectReference{
				Name:      pvcName,
				Namespace: pvcNamespace,
				UID:       types.UID(pvcUid),
			},
		},
	}
}

func populateOperationStartError(ctx context.Context, p *PopulatorParams) error {
	return fmt.Errorf(testPopulationOperationStartFailed)
}

func PopulateOperationStartSuccess(ctx context.Context, p *PopulatorParams) error {
	return nil
}

func populateCompleteError(ctx context.Context, p *PopulatorParams) (bool, error) {
	return false, fmt.Errorf(testPopulateCompleteFailed)
}

func populateNotComplete(ctx context.Context, p *PopulatorParams) (bool, error) {
	return false, nil
}

func populateCompleteSuccess(ctx context.Context, p *PopulatorParams) (bool, error) {
	return true, nil
}

func initTest(useProviderImpl bool, populate func(context.Context, *PopulatorParams) error, populateComplete func(context.Context, *PopulatorParams) (bool, error)) (
	*controller,
	informercorev1.PersistentVolumeClaimInformer,
	cache.SharedIndexInformer,
	informerstoragev1.StorageClassInformer,
	informercorev1.PodInformer,
	informercorev1.PersistentVolumeInformer,
) {
	gvr := schema.GroupVersionResource{
		Group:    testApiGroup,
		Version:  "v1alpha1",
		Resource: "testdatasources",
	}
	gk := schema.GroupKind{
		Group: testApiGroup,
		Kind:  testDatasourceKind,
	}

	kubeClient := kubefake.NewSimpleClientset()
	dynClient := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	gatewayClient := gatewayfake.NewSimpleClientset()

	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(kubeClient, time.Second*30)
	dynInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, time.Second*30)

	pvcInformer := kubeInformerFactory.Core().V1().PersistentVolumeClaims()
	pvInformer := kubeInformerFactory.Core().V1().PersistentVolumes()
	podInformer := kubeInformerFactory.Core().V1().Pods()
	scInformer := kubeInformerFactory.Storage().V1().StorageClasses()
	unstInformer := dynInformerFactory.ForResource(gvr).Informer()

	gatewayInformerFactory := gatewayInformers.NewSharedInformerFactory(gatewayClient, time.Second*30)
	referenceGrants := gatewayInformerFactory.Gateway().V1beta1().ReferenceGrants()

	populatorArgs := func(b bool, u *unstructured.Unstructured) ([]string, error) {
		var args []string
		return args, nil
	}

	c := &controller{
		kubeClient:           kubeClient,
		imageName:            "",
		populatorNamespace:   testVpWorkingNamespace,
		devicePath:           "",
		mountPath:            "",
		populatedFromAnno:    testPrefix + "/" + populatedFromAnnoSuffix,
		pvcFinalizer:         testPrefix + "/" + pvcFinalizerSuffix,
		pvcLister:            pvcInformer.Lister(),
		pvcSynced:            pvcInformer.Informer().HasSynced,
		pvLister:             pvInformer.Lister(),
		pvSynced:             pvInformer.Informer().HasSynced,
		podLister:            podInformer.Lister(),
		podSynced:            podInformer.Informer().HasSynced,
		scLister:             scInformer.Lister(),
		scSynced:             scInformer.Informer().HasSynced,
		unstLister:           dynamiclister.New(unstInformer.GetIndexer(), gvr),
		unstSynced:           unstInformer.HasSynced,
		notifyMap:            make(map[string]*stringSet),
		cleanupMap:           make(map[string]*stringSet),
		workqueue:            workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		populatorArgs:        populatorArgs,
		gk:                   gk,
		metrics:              initMetrics(),
		recorder:             getRecorder(kubeClient, testPrefix+"-"+controllerNameSuffix),
		referenceGrantLister: referenceGrants.Lister(),
		referenceGrantSynced: referenceGrants.Informer().HasSynced,
		useProviderImpl:      useProviderImpl,
		populate:             populate,
		populateComplete:     populateComplete,
	}
	return c, pvcInformer, unstInformer, scInformer, podInformer, pvInformer
}

func compareResult(want error, got error) bool {
	if want == nil {
		return got == nil
	}
	if got == nil {
		return want == nil
	}
	return want.Error() == got.Error()
}

func compareNotifyMap(want []string, got map[string]*stringSet) error {
	if len(want) != len(got) {
		return fmt.Errorf("The number of keys expected is different from actual. Expect %v, got %v", len(want), len(got))
	}
	for _, key := range want {
		if got[key] == nil {
			return fmt.Errorf("Expected key %s not found in the notifyMap", key)
		}
	}
	return nil
}

func runSyncPvcTests(tests []testCase, t *testing.T) {
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, pvcInformer, unstInformer, scInformer, podInformer, pvInformer := initTest(test.useProviderImpl, test.populate, test.populateComplete)
			for _, obj := range test.initialObjects {
				switch obj.(type) {
				case *v1.PersistentVolumeClaim:
					pvc := obj.(*v1.PersistentVolumeClaim)
					_, err := c.kubeClient.CoreV1().PersistentVolumeClaims(pvc.ObjectMeta.Namespace).Create(context.TODO(), pvc, metav1.CreateOptions{})
					if err != nil {
						t.Fatalf("Create pvc failed: %s", err.Error())
					}
					pvcInformer.Informer().GetStore().Add(obj)
				case *unstructured.Unstructured:
					unstInformer.GetStore().Add(obj)
				case *storagev1.StorageClass:
					scInformer.Informer().GetStore().Add(obj)
				case *v1.Pod:
					pod := obj.(*v1.Pod)
					_, err := c.kubeClient.CoreV1().Pods(pod.ObjectMeta.Namespace).Create(context.TODO(), pod, metav1.CreateOptions{})
					if err != nil {
						t.Fatalf("Create pod failed: %s", err.Error())
					}
					podInformer.Informer().GetStore().Add(obj)
				case *v1.PersistentVolume:
					pv := obj.(*v1.PersistentVolume)
					_, err := c.kubeClient.CoreV1().PersistentVolumes().Create(context.TODO(), pv, metav1.CreateOptions{})
					if err != nil {
						t.Fatalf("Create pv failed: %s", err.Error())
					}
					pvInformer.Informer().GetStore().Add(obj)
				default:
					t.Fatalf("Unknown initalObject type: %+v", obj)
				}
			}

			result := c.syncPvc(context.TODO(), test.key, test.pvcNamespace, test.pvcName)
			if !compareResult(test.expectedResult, result) {
				t.Errorf("Error: expected result %t, got %t", test.expectedResult, result)
			}
			err := compareNotifyMap(test.expectedKeys, c.notifyMap)
			if err != nil {
				t.Errorf(err.Error())
			}
		})
	}
}

func TestSyncPvcWithPopulatorPod(t *testing.T) {
	tests := []testCase{
		{
			name:         "Ignore PVCs in controller's working namespace",
			key:          "pvc/" + testVpWorkingNamespace + "/" + testPvcName,
			pvcNamespace: testVpWorkingNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testVpWorkingNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testVpWorkingNamespace), ""),
			},
			useProviderImpl: false,
			expectedResult:  nil,
			expectedKeys:    []string{},
		},
		{
			name:            "Orginal PVC not found",
			key:             "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace:    testPvcNamespace,
			pvcName:         testPvcName,
			initialObjects:  []runtime.Object{},
			useProviderImpl: false,
			expectedResult:  nil,
			expectedKeys:    []string{},
		},
		{
			name:            "Ignore PVCs without a data source",
			key:             "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace:    testPvcNamespace,
			pvcName:         testPvcName,
			initialObjects:  []runtime.Object{pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "", nil, "")},
			useProviderImpl: false,
			expectedResult:  nil,
			expectedKeys:    []string{},
		},
		{
			name:         "Ignore PVCs that aren't for this populator to handle, apiGroup not match",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf("test.api.group1", testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
			},
			useProviderImpl: false,
			expectedResult:  nil,
			expectedKeys:    []string{},
		},
		{
			name:         "Ignore PVCs that aren't for this populator to handle, kind not match",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, "TestKind1", testDataSourceName, testPvcNamespace), ""),
			},
			useProviderImpl: false,
			expectedResult:  nil,
			expectedKeys:    []string{},
		},
		{
			name:         "Ignore PVCs that aren't for this populator to handle, data source name not exist",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, "", testPvcNamespace), ""),
			},
			useProviderImpl: false,
			expectedResult:  nil,
			expectedKeys:    []string{},
		},
		{
			name:         "Original PVC and data source in different namespace without grant",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, "default1"), ""),
			},
			useProviderImpl: false,
			expectedResult:  errors.New("accessing default1/test-data-source-name of TestKind dataSource from default/test-pvc isn't allowed"),
			expectedKeys:    []string{},
		},
		{
			name:         "Data source not exists",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
			},
			useProviderImpl: false,
			expectedResult:  nil,
			expectedKeys:    []string{dataSourceKey},
		},
		{
			name:         "StorageClass not exists",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
				ust(),
			},
			useProviderImpl: false,
			expectedResult:  nil,
			expectedKeys:    []string{storageClassKey},
		},
		{
			name:         "PVC not bound to a node",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, "", testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
				ust(),
				sc(testStorageClassName, storagev1.VolumeBindingWaitForFirstConsumer),
			},
			useProviderImpl: false,
			expectedResult:  nil,
			expectedKeys:    []string{},
		},
		{
			name:         "Create populator pod",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
				ust(),
				sc(testStorageClassName, storagev1.VolumeBindingWaitForFirstConsumer),
			},
			useProviderImpl: false,
			expectedResult:  nil,
			expectedKeys:    []string{podKey, pvcPrimeKey},
		},
		{
			name:         "Wait populator pod succeed",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
				ust(),
				sc(testStorageClassName, storagev1.VolumeBindingWaitForFirstConsumer),
				pod(corev1.PodRunning),
			},
			useProviderImpl: false,
			expectedResult:  nil,
			expectedKeys:    []string{podKey, pvcPrimeKey},
		},
		{
			name:         "Populator pod failed",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
				ust(),
				sc(testStorageClassName, storagev1.VolumeBindingWaitForFirstConsumer),
				pod(corev1.PodFailed),
			},
			useProviderImpl: false,
			expectedResult:  nil,
			expectedKeys:    []string{podKey, pvcPrimeKey},
		},
		{
			name:         "PV not exists",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
				ust(),
				sc(testStorageClassName, storagev1.VolumeBindingWaitForFirstConsumer),
				pod(corev1.PodSucceeded),
				pvc(testPopulatorPvcName, testVpWorkingNamespace, "", testStorageClassName, testPvName, nil, ""),
			},
			useProviderImpl: false,
			expectedResult:  nil,
			expectedKeys:    []string{podKey, pvcPrimeKey, pvKey},
		},
		{
			name:         "Wait for the bind controller to rebind the PV",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
				ust(),
				sc(testStorageClassName, storagev1.VolumeBindingWaitForFirstConsumer),
				pod(corev1.PodSucceeded),
				pvc(testPopulatorPvcName, testVpWorkingNamespace, "", testStorageClassName, testPvName, nil, ""),
				pv(testPvcName, testPvcNamespace, testPvcUid),
			},
			useProviderImpl: false,
			expectedResult:  nil,
			expectedKeys:    []string{podKey, pvcPrimeKey, pvKey},
		},
		{
			name:         "Clean up populator pod and pvcPrime",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
				ust(),
				sc(testStorageClassName, storagev1.VolumeBindingWaitForFirstConsumer),
				pod(corev1.PodSucceeded),
				pvc(testPopulatorPvcName, testVpWorkingNamespace, "", testStorageClassName, testPvName, nil, corev1.ClaimLost),
				pv(testPvcName, testPvcNamespace, testPvcUid),
			},
			useProviderImpl: false,
			expectedResult:  nil,
			expectedKeys:    []string{},
		},
	}

	runSyncPvcTests(tests, t)
}

func TestSyncPvcWithCloudProviderImplementation(t *testing.T) {
	tests := []testCase{
		{
			name:         "Ignore PVCs in controller's working namespace",
			key:          "pvc/" + testVpWorkingNamespace + "/" + testPvcName,
			pvcNamespace: testVpWorkingNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testVpWorkingNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testVpWorkingNamespace), ""),
			},
			useProviderImpl: true,
			expectedResult:  nil,
			expectedKeys:    []string{},
		},
		{
			name:            "Orginal PVC not found",
			key:             "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace:    testPvcNamespace,
			pvcName:         testPvcName,
			initialObjects:  []runtime.Object{},
			useProviderImpl: true,
			expectedResult:  nil,
			expectedKeys:    []string{},
		},
		{
			name:            "Ignore PVCs without a data source",
			key:             "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace:    testPvcNamespace,
			pvcName:         testPvcName,
			initialObjects:  []runtime.Object{pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "", nil, "")},
			useProviderImpl: true,
			expectedResult:  nil,
			expectedKeys:    []string{},
		},
		{
			name:         "Ignore PVCs that aren't for this populator to handle, apiGroup not match",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf("test.api.group1", testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
			},
			useProviderImpl: true,
			expectedResult:  nil,
			expectedKeys:    []string{},
		},
		{
			name:         "Ignore PVCs that aren't for this populator to handle, kind not match",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, "TestKind1", testDataSourceName, testPvcNamespace), ""),
			},
			useProviderImpl: true,
			expectedResult:  nil,
			expectedKeys:    []string{},
		},
		{
			name:         "Ignore PVCs that aren't for this populator to handle, data source name not exist",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, "", testPvcNamespace), ""),
			},
			useProviderImpl: true,
			expectedResult:  nil,
			expectedKeys:    []string{},
		},
		{
			name:         "Original PVC and data source in different namespace without grant",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, "default1"), ""),
			},
			useProviderImpl: true,
			expectedResult:  errors.New("accessing default1/test-data-source-name of TestKind dataSource from default/test-pvc isn't allowed"),
			expectedKeys:    []string{},
		},
		{
			name:         "Data source not exists",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
			},
			useProviderImpl: true,
			expectedResult:  nil,
			expectedKeys:    []string{dataSourceKey},
		},
		{
			name:         "StorageClass not exists",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
				ust(),
			},
			useProviderImpl: true,
			expectedResult:  nil,
			expectedKeys:    []string{storageClassKey},
		},
		{
			name:         "PVC not bound to a node",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, "", testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
				ust(),
				sc(testStorageClassName, storagev1.VolumeBindingWaitForFirstConsumer),
			},
			useProviderImpl: true,
			expectedResult:  nil,
			expectedKeys:    []string{},
		},
		{
			name:         "Create PVC prime",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
				ust(),
				sc(testStorageClassName, storagev1.VolumeBindingWaitForFirstConsumer),
				sc(testStorageClassPrimeName, storagev1.VolumeBindingImmediate),
			},
			useProviderImpl: true,
			expectedResult:  nil,
			expectedKeys:    []string{pvcPrimeKey},
		},
		{
			name:         "Populate operation start return an error",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
				ust(),
				sc(testStorageClassName, storagev1.VolumeBindingWaitForFirstConsumer),
				sc(testStorageClassPrimeName, storagev1.VolumeBindingImmediate),
				pvc(testPopulatorPvcName, testVpWorkingNamespace, "", testStorageClassName, testPvName, nil, corev1.ClaimBound),
			},
			useProviderImpl: true,
			populate:        populateOperationStartError,
			expectedResult:  fmt.Errorf(testPopulationOperationStartFailed),
			expectedKeys:    []string{pvcPrimeKey},
		},
		{
			name:         "Populate completeness check return an error",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
				ust(),
				sc(testStorageClassName, storagev1.VolumeBindingWaitForFirstConsumer),
				sc(testStorageClassPrimeName, storagev1.VolumeBindingImmediate),
				pvc(testPopulatorPvcName, testVpWorkingNamespace, "", testStorageClassName, testPvName, nil, corev1.ClaimBound),
			},
			useProviderImpl:  true,
			populate:         PopulateOperationStartSuccess,
			populateComplete: populateCompleteError,
			expectedResult:   fmt.Errorf(testPopulateCompleteFailed),
			expectedKeys:     []string{pvcPrimeKey},
		},
		{
			name:         "Populate not complete",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
				ust(),
				sc(testStorageClassName, storagev1.VolumeBindingWaitForFirstConsumer),
				sc(testStorageClassPrimeName, storagev1.VolumeBindingImmediate),
				pvc(testPopulatorPvcName, testVpWorkingNamespace, "", testStorageClassName, testPvName, nil, corev1.ClaimBound),
			},
			useProviderImpl:  true,
			populate:         PopulateOperationStartSuccess,
			populateComplete: populateNotComplete,
			expectedResult:   nil,
			expectedKeys:     []string{pvcPrimeKey},
		},
		{
			name:         "PV not exists",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
				ust(),
				sc(testStorageClassName, storagev1.VolumeBindingWaitForFirstConsumer),
				sc(testStorageClassPrimeName, storagev1.VolumeBindingImmediate),
				pvc(testPopulatorPvcName, testVpWorkingNamespace, "", testStorageClassName, testPvName, nil, ""),
			},
			useProviderImpl:  true,
			populate:         PopulateOperationStartSuccess,
			populateComplete: populateCompleteSuccess,
			expectedResult:   nil,
			expectedKeys:     []string{pvcPrimeKey, pvKey},
		},
		{
			name:         "Wait for the bind controller to rebind the PV",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
				ust(),
				sc(testStorageClassName, storagev1.VolumeBindingWaitForFirstConsumer),
				sc(testStorageClassPrimeName, storagev1.VolumeBindingImmediate),
				pvc(testPopulatorPvcName, testVpWorkingNamespace, "", testStorageClassName, testPvName, nil, ""),
				pv(testPvcName, testPvcNamespace, testPvcUid),
			},
			useProviderImpl:  true,
			populate:         PopulateOperationStartSuccess,
			populateComplete: populateCompleteSuccess,
			expectedResult:   nil,
			expectedKeys:     []string{pvcPrimeKey, pvKey},
		},
		{
			name:         "Clean up pvcPrime",
			key:          "pvc/" + testPvcNamespace + "/" + testPvcName,
			pvcNamespace: testPvcNamespace,
			pvcName:      testPvcName,
			initialObjects: []runtime.Object{
				pvc(testPvcName, testPvcNamespace, testNodeName, testStorageClassName, "",
					dsf(testApiGroup, testDatasourceKind, testDataSourceName, testPvcNamespace), ""),
				ust(),
				sc(testStorageClassName, storagev1.VolumeBindingWaitForFirstConsumer),
				sc(testStorageClassPrimeName, storagev1.VolumeBindingImmediate),
				pvc(testPopulatorPvcName, testVpWorkingNamespace, "", testStorageClassName, testPvName, nil, corev1.ClaimLost),
				pv(testPvcName, testPvcNamespace, testPvcUid),
			},
			useProviderImpl:  true,
			populate:         PopulateOperationStartSuccess,
			populateComplete: populateCompleteSuccess,
			expectedResult:   nil,
			expectedKeys:     []string{},
		},
	}

	runSyncPvcTests(tests, t)
}
