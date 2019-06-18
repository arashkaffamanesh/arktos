/*
Copyright 2015 The Kubernetes Authors.

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

package scheduling

import (
	"time"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/test/e2e/framework"
	e2elog "k8s.io/kubernetes/test/e2e/framework/log"
	e2enode "k8s.io/kubernetes/test/e2e/framework/node"
	testutils "k8s.io/kubernetes/test/utils"
	imageutils "k8s.io/kubernetes/test/utils/image"

	"github.com/onsi/ginkgo"
	// ensure libs have a chance to initialize
	_ "github.com/stretchr/testify/assert"
)

var (
	pauseImage = imageutils.GetE2EImage(imageutils.Pause)
)

func getTestTaint() v1.Taint {
	now := metav1.Now()
	return v1.Taint{
		Key:       "kubernetes.io/e2e-evict-taint-key",
		Value:     "evictTaintVal",
		Effect:    v1.TaintEffectNoExecute,
		TimeAdded: &now,
	}
}

// Create a default pod for this test, with argument saying if the Pod should have
// toleration for Taits used in this test.
func createPodForTaintsTest(hasToleration bool, tolerationSeconds int, podName, podLabel, ns string) *v1.Pod {
	grace := int64(1)
	if !hasToleration {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:                       podName,
				Namespace:                  ns,
				Labels:                     map[string]string{"group": podLabel},
				DeletionGracePeriodSeconds: &grace,
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:  "pause",
						Image: pauseImage,
					},
				},
			},
		}
	}
	if tolerationSeconds <= 0 {
		return &v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:                       podName,
				Namespace:                  ns,
				Labels:                     map[string]string{"group": podLabel},
				DeletionGracePeriodSeconds: &grace,
				// default - tolerate forever
			},
			Spec: v1.PodSpec{
				Containers: []v1.Container{
					{
						Name:  "pause",
						Image: pauseImage,
					},
				},
				Tolerations: []v1.Toleration{{Key: "kubernetes.io/e2e-evict-taint-key", Value: "evictTaintVal", Effect: v1.TaintEffectNoExecute}},
			},
		}
	}
	ts := int64(tolerationSeconds)
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:                       podName,
			Namespace:                  ns,
			Labels:                     map[string]string{"group": podLabel},
			DeletionGracePeriodSeconds: &grace,
		},
		Spec: v1.PodSpec{
			Containers: []v1.Container{
				{
					Name:  "pause",
					Image: pauseImage,
				},
			},
			// default - tolerate forever
			Tolerations: []v1.Toleration{{Key: "kubernetes.io/e2e-evict-taint-key", Value: "evictTaintVal", Effect: v1.TaintEffectNoExecute, TolerationSeconds: &ts}},
		},
	}
}

// Creates and starts a controller (informer) that watches updates on a pod in given namespace with given name. It puts a new
// struct into observedDeletion channel for every deletion it sees.
func createTestController(cs clientset.Interface, observedDeletions chan string, stopCh chan struct{}, podLabel, ns string) {
	_, controller := cache.NewInformer(
		&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				options.LabelSelector = labels.SelectorFromSet(labels.Set{"group": podLabel}).String()
				obj, err := cs.CoreV1().Pods(ns).List(options)
				return runtime.Object(obj), err
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				options.LabelSelector = labels.SelectorFromSet(labels.Set{"group": podLabel}).String()
				return cs.CoreV1().Pods(ns).Watch(options)
			},
		},
		&v1.Pod{},
		0,
		cache.ResourceEventHandlerFuncs{
			DeleteFunc: func(oldObj interface{}) {
				if delPod, ok := oldObj.(*v1.Pod); ok {
					observedDeletions <- delPod.Name
				} else {
					observedDeletions <- ""
				}
			},
		},
	)
	e2elog.Logf("Starting informer...")
	go controller.Run(stopCh)
}

const (
	kubeletPodDeletionDelaySeconds = 60
	additionalWaitPerDeleteSeconds = 5
)

// Tests the behavior of NoExecuteTaintManager. Following scenarios are included:
// - eviction of non-tolerating pods from a tainted node,
// - lack of eviction of tolerating pods from a tainted node,
// - delayed eviction of short-tolerating pod from a tainted node,
// - lack of eviction of short-tolerating pod after taint removal.
var _ = SIGDescribe("NoExecuteTaintManager Single Pod [Serial]", func() {
	var cs clientset.Interface
	var ns string
	f := framework.NewDefaultFramework("taint-single-pod")

	ginkgo.BeforeEach(func() {
		cs = f.ClientSet
		ns = f.Namespace.Name

		e2enode.WaitForTotalHealthy(cs, time.Minute)

		err := framework.CheckTestingNSDeletedExcept(cs, ns)
		framework.ExpectNoError(err)
	})

	// 1. Run a pod
	// 2. Taint the node running this pod with a no-execute taint
	// 3. See if pod will get evicted
	ginkgo.It("evicts pods from tainted nodes", func() {
		podName := "taint-eviction-1"
		pod := createPodForTaintsTest(false, 0, podName, podName, ns)
		observedDeletions := make(chan string, 100)
		stopCh := make(chan struct{})
		createTestController(cs, observedDeletions, stopCh, podName, ns)

		ginkgo.By("Starting pod...")
		nodeName, err := testutils.RunPodAndGetNodeName(cs, pod, 2*time.Minute)
		framework.ExpectNoError(err)
		e2elog.Logf("Pod is running on %v. Tainting Node", nodeName)

		ginkgo.By("Trying to apply a taint on the Node")
		testTaint := getTestTaint()
		framework.AddOrUpdateTaintOnNode(cs, nodeName, testTaint)
		framework.ExpectNodeHasTaint(cs, nodeName, &testTaint)
		defer framework.RemoveTaintOffNode(cs, nodeName, testTaint)

		// Wait a bit
		ginkgo.By("Waiting for Pod to be deleted")
		timeoutChannel := time.NewTimer(time.Duration(kubeletPodDeletionDelaySeconds+additionalWaitPerDeleteSeconds) * time.Second).C
		select {
		case <-timeoutChannel:
			framework.Failf("Failed to evict Pod")
		case <-observedDeletions:
			e2elog.Logf("Noticed Pod eviction. Test successful")
		}
	})

	// 1. Run a pod with toleration
	// 2. Taint the node running this pod with a no-execute taint
	// 3. See if pod won't get evicted
	ginkgo.It("doesn't evict pod with tolerations from tainted nodes", func() {
		podName := "taint-eviction-2"
		pod := createPodForTaintsTest(true, 0, podName, podName, ns)
		observedDeletions := make(chan string, 100)
		stopCh := make(chan struct{})
		createTestController(cs, observedDeletions, stopCh, podName, ns)

		ginkgo.By("Starting pod...")
		nodeName, err := testutils.RunPodAndGetNodeName(cs, pod, 2*time.Minute)
		framework.ExpectNoError(err)
		e2elog.Logf("Pod is running on %v. Tainting Node", nodeName)

		ginkgo.By("Trying to apply a taint on the Node")
		testTaint := getTestTaint()
		framework.AddOrUpdateTaintOnNode(cs, nodeName, testTaint)
		framework.ExpectNodeHasTaint(cs, nodeName, &testTaint)
		defer framework.RemoveTaintOffNode(cs, nodeName, testTaint)

		// Wait a bit
		ginkgo.By("Waiting for Pod to be deleted")
		timeoutChannel := time.NewTimer(time.Duration(kubeletPodDeletionDelaySeconds+additionalWaitPerDeleteSeconds) * time.Second).C
		select {
		case <-timeoutChannel:
			e2elog.Logf("Pod wasn't evicted. Test successful")
		case <-observedDeletions:
			framework.Failf("Pod was evicted despite toleration")
		}
	})

	// 1. Run a pod with a finite toleration
	// 2. Taint the node running this pod with a no-execute taint
	// 3. See if pod won't get evicted before toleration time runs out
	// 4. See if pod will get evicted after toleration time runs out
	ginkgo.It("eventually evict pod with finite tolerations from tainted nodes", func() {
		podName := "taint-eviction-3"
		pod := createPodForTaintsTest(true, kubeletPodDeletionDelaySeconds+2*additionalWaitPerDeleteSeconds, podName, podName, ns)
		observedDeletions := make(chan string, 100)
		stopCh := make(chan struct{})
		createTestController(cs, observedDeletions, stopCh, podName, ns)

		ginkgo.By("Starting pod...")
		nodeName, err := testutils.RunPodAndGetNodeName(cs, pod, 2*time.Minute)
		framework.ExpectNoError(err)
		e2elog.Logf("Pod is running on %v. Tainting Node", nodeName)

		ginkgo.By("Trying to apply a taint on the Node")
		testTaint := getTestTaint()
		framework.AddOrUpdateTaintOnNode(cs, nodeName, testTaint)
		framework.ExpectNodeHasTaint(cs, nodeName, &testTaint)
		defer framework.RemoveTaintOffNode(cs, nodeName, testTaint)

		// Wait a bit
		ginkgo.By("Waiting to see if a Pod won't be deleted")
		timeoutChannel := time.NewTimer(time.Duration(kubeletPodDeletionDelaySeconds+additionalWaitPerDeleteSeconds) * time.Second).C
		select {
		case <-timeoutChannel:
			e2elog.Logf("Pod wasn't evicted")
		case <-observedDeletions:
			framework.Failf("Pod was evicted despite toleration")
			return
		}
		ginkgo.By("Waiting for Pod to be deleted")
		timeoutChannel = time.NewTimer(time.Duration(kubeletPodDeletionDelaySeconds+additionalWaitPerDeleteSeconds) * time.Second).C
		select {
		case <-timeoutChannel:
			framework.Failf("Pod wasn't evicted")
		case <-observedDeletions:
			e2elog.Logf("Pod was evicted after toleration time run out. Test successful")
			return
		}
	})

	// 1. Run a pod with short toleration
	// 2. Taint the node running this pod with a no-execute taint
	// 3. Wait some time
	// 4. Remove the taint
	// 5. See if Pod won't be evicted.
	ginkgo.It("removing taint cancels eviction", func() {
		podName := "taint-eviction-4"
		pod := createPodForTaintsTest(true, 2*additionalWaitPerDeleteSeconds, podName, podName, ns)
		observedDeletions := make(chan string, 100)
		stopCh := make(chan struct{})
		createTestController(cs, observedDeletions, stopCh, podName, ns)

		ginkgo.By("Starting pod...")
		nodeName, err := testutils.RunPodAndGetNodeName(cs, pod, 2*time.Minute)
		framework.ExpectNoError(err)
		e2elog.Logf("Pod is running on %v. Tainting Node", nodeName)

		ginkgo.By("Trying to apply a taint on the Node")
		testTaint := getTestTaint()
		framework.AddOrUpdateTaintOnNode(cs, nodeName, testTaint)
		framework.ExpectNodeHasTaint(cs, nodeName, &testTaint)
		taintRemoved := false
		defer func() {
			if !taintRemoved {
				framework.RemoveTaintOffNode(cs, nodeName, testTaint)
			}
		}()

		// Wait a bit
		ginkgo.By("Waiting short time to make sure Pod is queued for deletion")
		timeoutChannel := time.NewTimer(additionalWaitPerDeleteSeconds).C
		select {
		case <-timeoutChannel:
			e2elog.Logf("Pod wasn't evicted. Proceeding")
		case <-observedDeletions:
			framework.Failf("Pod was evicted despite toleration")
			return
		}
		e2elog.Logf("Removing taint from Node")
		framework.RemoveTaintOffNode(cs, nodeName, testTaint)
		taintRemoved = true
		ginkgo.By("Waiting some time to make sure that toleration time passed.")
		timeoutChannel = time.NewTimer(time.Duration(kubeletPodDeletionDelaySeconds+3*additionalWaitPerDeleteSeconds) * time.Second).C
		select {
		case <-timeoutChannel:
			e2elog.Logf("Pod wasn't evicted. Test successful")
		case <-observedDeletions:
			framework.Failf("Pod was evicted despite toleration")
		}
	})
})

var _ = SIGDescribe("NoExecuteTaintManager Multiple Pods [Serial]", func() {
	var cs clientset.Interface
	var ns string
	f := framework.NewDefaultFramework("taint-multiple-pods")

	ginkgo.BeforeEach(func() {
		cs = f.ClientSet
		ns = f.Namespace.Name

		e2enode.WaitForTotalHealthy(cs, time.Minute)

		err := framework.CheckTestingNSDeletedExcept(cs, ns)
		framework.ExpectNoError(err)
	})

	// 1. Run two pods; one with toleration, one without toleration
	// 2. Taint the nodes running those pods with a no-execute taint
	// 3. See if pod-without-toleration get evicted, and pod-with-toleration is kept
	ginkgo.It("only evicts pods without tolerations from tainted nodes", func() {
		podGroup := "taint-eviction-a"
		observedDeletions := make(chan string, 100)
		stopCh := make(chan struct{})
		createTestController(cs, observedDeletions, stopCh, podGroup, ns)

		pod1 := createPodForTaintsTest(false, 0, podGroup+"1", podGroup, ns)
		pod2 := createPodForTaintsTest(true, 0, podGroup+"2", podGroup, ns)

		ginkgo.By("Starting pods...")
		nodeName1, err := testutils.RunPodAndGetNodeName(cs, pod1, 2*time.Minute)
		framework.ExpectNoError(err)
		e2elog.Logf("Pod1 is running on %v. Tainting Node", nodeName1)
		nodeName2, err := testutils.RunPodAndGetNodeName(cs, pod2, 2*time.Minute)
		framework.ExpectNoError(err)
		e2elog.Logf("Pod2 is running on %v. Tainting Node", nodeName2)

		ginkgo.By("Trying to apply a taint on the Nodes")
		testTaint := getTestTaint()
		framework.AddOrUpdateTaintOnNode(cs, nodeName1, testTaint)
		framework.ExpectNodeHasTaint(cs, nodeName1, &testTaint)
		defer framework.RemoveTaintOffNode(cs, nodeName1, testTaint)
		if nodeName2 != nodeName1 {
			framework.AddOrUpdateTaintOnNode(cs, nodeName2, testTaint)
			framework.ExpectNodeHasTaint(cs, nodeName2, &testTaint)
			defer framework.RemoveTaintOffNode(cs, nodeName2, testTaint)
		}

		// Wait a bit
		ginkgo.By("Waiting for Pod1 to be deleted")
		timeoutChannel := time.NewTimer(time.Duration(kubeletPodDeletionDelaySeconds+additionalWaitPerDeleteSeconds) * time.Second).C
		var evicted int
		for {
			select {
			case <-timeoutChannel:
				if evicted == 0 {
					framework.Failf("Failed to evict Pod1.")
				} else if evicted == 2 {
					framework.Failf("Pod1 is evicted. But unexpected Pod2 also get evicted.")
				}
				return
			case podName := <-observedDeletions:
				evicted++
				if podName == podGroup+"1" {
					e2elog.Logf("Noticed Pod %q gets evicted.", podName)
				} else if podName == podGroup+"2" {
					framework.Failf("Unexepected Pod %q gets evicted.", podName)
					return
				}
			}
		}
	})

	// 1. Run two pods both with toleration; one with tolerationSeconds=5, the other with 25
	// 2. Taint the nodes running those pods with a no-execute taint
	// 3. See if both pods get evicted in between [5, 25] seconds
	ginkgo.It("evicts pods with minTolerationSeconds", func() {
		podGroup := "taint-eviction-b"
		observedDeletions := make(chan string, 100)
		stopCh := make(chan struct{})
		createTestController(cs, observedDeletions, stopCh, podGroup, ns)

		pod1 := createPodForTaintsTest(true, additionalWaitPerDeleteSeconds, podGroup+"1", podGroup, ns)
		pod2 := createPodForTaintsTest(true, 5*additionalWaitPerDeleteSeconds, podGroup+"2", podGroup, ns)

		ginkgo.By("Starting pods...")
		nodeName, err := testutils.RunPodAndGetNodeName(cs, pod1, 2*time.Minute)
		node, err := cs.CoreV1().Nodes().Get(nodeName, metav1.GetOptions{})
		framework.ExpectNoError(err)
		nodeHostNameLabel, ok := node.GetObjectMeta().GetLabels()["kubernetes.io/hostname"]
		if !ok {
			framework.Failf("error getting kubernetes.io/hostname label on node %s", nodeName)
		}
		framework.ExpectNoError(err)
		e2elog.Logf("Pod1 is running on %v. Tainting Node", nodeName)
		// ensure pod2 lands on the same node as pod1
		pod2.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": nodeHostNameLabel}
		_, err = testutils.RunPodAndGetNodeName(cs, pod2, 2*time.Minute)
		framework.ExpectNoError(err)
		e2elog.Logf("Pod2 is running on %v. Tainting Node", nodeName)

		ginkgo.By("Trying to apply a taint on the Node")
		testTaint := getTestTaint()
		framework.AddOrUpdateTaintOnNode(cs, nodeName, testTaint)
		framework.ExpectNodeHasTaint(cs, nodeName, &testTaint)
		defer framework.RemoveTaintOffNode(cs, nodeName, testTaint)

		// Wait a bit
		ginkgo.By("Waiting for Pod1 and Pod2 to be deleted")
		timeoutChannel := time.NewTimer(time.Duration(kubeletPodDeletionDelaySeconds+3*additionalWaitPerDeleteSeconds) * time.Second).C
		var evicted int
		for evicted != 2 {
			select {
			case <-timeoutChannel:
				framework.Failf("Failed to evict all Pods. %d pod(s) is not evicted.", 2-evicted)
				return
			case podName := <-observedDeletions:
				e2elog.Logf("Noticed Pod %q gets evicted.", podName)
				evicted++
			}
		}
	})
})
