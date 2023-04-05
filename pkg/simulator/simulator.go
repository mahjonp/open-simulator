package simulator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pterm/pterm"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/informers"
	kubeinformers "k8s.io/client-go/informers"
	externalclientset "k8s.io/client-go/kubernetes"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/scheduler"
	kubeschedulerconfig "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	utiltrace "k8s.io/utils/trace"

	simonplugin "github.com/alibaba/open-simulator/pkg/simulator/plugin"
	"github.com/alibaba/open-simulator/pkg/test"
	simontype "github.com/alibaba/open-simulator/pkg/type"
	"github.com/alibaba/open-simulator/pkg/utils"
	"k8s.io/client-go/tools/events"
)

// Simulator is used to simulate a cluster and pods scheduling
type Simulator struct {
	// kube client
	kubeclient      externalclientset.Interface
	fakeclient      externalclientset.Interface
	informerFactory informers.SharedInformerFactory

	// scheduler
	scheduler *scheduler.Scheduler

	// stopCh
	simulatorStop chan struct{}

	// context
	ctx                   context.Context
	cancelFunc            context.CancelFunc
	scheduleOneCtx        context.Context
	scheduleOneCancelFunc context.CancelFunc

	eventBroadcaster events.EventBroadcasterAdapter

	disablePTerm    bool
	patchPodFuncMap PatchPodsFuncMap

	status status
}

// status captures reason why one pod fails to be scheduled
type status struct {
	stopReason string
}

type PatchPodFunc = func(pods []*corev1.Pod, client externalclientset.Interface) error

type PatchPodsFuncMap map[string]PatchPodFunc

type simulatorOptions struct {
	kubeconfig         string
	schedulerConfig    string
	scheduleConfigOpts []ScheduleConfigOption
	disablePTerm       bool
	extraRegistry      frameworkruntime.Registry
	patchPodFuncMap    PatchPodsFuncMap
}

// Option configures a Simulator
type Option func(*simulatorOptions)
type ScheduleConfigOption func(kubeschedulerconfig.KubeSchedulerConfiguration)

var defaultSimulatorOptions = simulatorOptions{
	kubeconfig:         "",
	schedulerConfig:    "",
	disablePTerm:       false,
	scheduleConfigOpts: []ScheduleConfigOption{},
	extraRegistry:      make(map[string]frameworkruntime.PluginFactory),
	patchPodFuncMap:    make(map[string]PatchPodFunc),
}

// NewSimulator generates all components that will be needed to simulate scheduling and returns a complete simulator
func NewSimulator(ctx context.Context, opts ...Option) (*Simulator, error) {
	var err error
	// Step 0: configures a Simulator by opts
	options := defaultSimulatorOptions
	for _, opt := range opts {
		opt(&options)
	}

	// Step 1: get scheduler CompletedConfig and set the list of scheduler bind plugins to Simon.
	kubeSchedulerConfig := GetSchedulerConfig(options.scheduleConfigOpts...)

	// Step 2: create client
	fakeClient := fakeclientset.NewSimpleClientset()
	kubeSchedulerConfig.Client = fakeClient

	// Step 3: Create the simulator
	ctx, cancel := context.WithCancel(ctx)
	scheduleOneCtx, scheduleOneCancel := context.WithCancel(ctx)
	sim := &Simulator{
		fakeclient:            fakeClient,
		simulatorStop:         make(chan struct{}),
		ctx:                   ctx,
		cancelFunc:            cancel,
		scheduleOneCtx:        scheduleOneCtx,
		scheduleOneCancelFunc: scheduleOneCancel,
		disablePTerm:          options.disablePTerm,
		patchPodFuncMap:       options.patchPodFuncMap,
		eventBroadcaster:      kubeSchedulerConfig.EventBroadcaster,
	}

	// Step 4: create informer
	kubeInformerFactory := kubeinformers.NewSharedInformerFactory(sim.fakeclient, 0)
	scInformer := kubeInformerFactory.Storage().V1().StorageClasses().Informer()
	csiNodeInformer := kubeInformerFactory.Storage().V1().CSINodes().Informer()
	cmInformer := kubeInformerFactory.Core().V1().ConfigMaps().Informer()
	svcInformer := kubeInformerFactory.Core().V1().Services().Informer()
	podInformer := kubeInformerFactory.Core().V1().Pods().Informer()
	pdbInformer := kubeInformerFactory.Policy().V1beta1().PodDisruptionBudgets().Informer()
	pvcInformer := kubeInformerFactory.Core().V1().PersistentVolumeClaims().Informer()
	pvInformer := kubeInformerFactory.Core().V1().PersistentVolumes().Informer()
	rcInformer := kubeInformerFactory.Core().V1().ReplicationControllers().Informer()
	rsInformer := kubeInformerFactory.Apps().V1().ReplicaSets().Informer()
	stsInformer := kubeInformerFactory.Apps().V1().StatefulSets().Informer()
	nodeInformer := kubeInformerFactory.Core().V1().Nodes().Informer()
	dsInformer := kubeInformerFactory.Apps().V1().DaemonSets().Informer()
	deployInformer := kubeInformerFactory.Apps().V1().Deployments().Informer()

	// Step 5: add event handler for pods
	kubeInformerFactory.Core().V1().Pods().Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				if pod, ok := obj.(*corev1.Pod); ok && pod.Spec.SchedulerName == simontype.DefaultSchedulerName {
					return true
				}
				return false
			},
			Handler: cache.ResourceEventHandlerFuncs{
				// AddFunc: func(obj interface{}) {
				// 	if pod, ok := obj.(*corev1.Pod); ok {
				// 		fmt.Printf("test add pod %s/%s\n", pod.Namespace, pod.Name)
				// 	}
				// },
				UpdateFunc: func(oldObj, newObj interface{}) {
					if pod, ok := newObj.(*corev1.Pod); ok {
						// fmt.Printf("test update pod %s/%s\n", pod.Namespace, pod.Name)
						sim.update(pod)
					}
				},
			},
		},
	)
	sim.informerFactory = kubeInformerFactory

	// Step 6: start informer
	sim.informerFactory.Start(ctx.Done())
	cache.WaitForCacheSync(ctx.Done(),
		scInformer.HasSynced,
		csiNodeInformer.HasSynced,
		cmInformer.HasSynced,
		svcInformer.HasSynced,
		podInformer.HasSynced,
		pdbInformer.HasSynced,
		pvcInformer.HasSynced,
		pvInformer.HasSynced,
		rcInformer.HasSynced,
		rsInformer.HasSynced,
		stsInformer.HasSynced,
		nodeInformer.HasSynced,
		dsInformer.HasSynced,
		deployInformer.HasSynced,
	)

	// Step 7: create scheduler for sim
	bindRegistry := frameworkruntime.Registry{
		simontype.SimonPluginName: func(configuration runtime.Object, f framework.Handle) (framework.Plugin, error) {
			return simonplugin.NewSimonPlugin(sim.fakeclient, configuration, f)
		},
	}
	for name, plugin := range options.extraRegistry {
		bindRegistry[name] = plugin
	}

	sim.scheduler, err = scheduler.New(
		sim.fakeclient,
		sim.informerFactory,
		nil,
		GetRecorderFactory(kubeSchedulerConfig),
		sim.ctx.Done(),
		scheduler.WithProfiles(kubeSchedulerConfig.ComponentConfig.Profiles...),
		scheduler.WithPercentageOfNodesToScore(kubeSchedulerConfig.ComponentConfig.PercentageOfNodesToScore),
		scheduler.WithFrameworkOutOfTreeRegistry(bindRegistry),
		scheduler.WithPodMaxBackoffSeconds(kubeSchedulerConfig.ComponentConfig.PodMaxBackoffSeconds),
		scheduler.WithPodInitialBackoffSeconds(kubeSchedulerConfig.ComponentConfig.PodInitialBackoffSeconds),
		scheduler.WithExtenders(kubeSchedulerConfig.ComponentConfig.Extenders...),
	)
	if err != nil {
		return nil, err
	}

	return sim, nil
}

// RunCluster
func (sim *Simulator) RunCluster(cluster ResourceTypes) (*SimulateResult, error) {
	// start scheduler
	sim.runScheduler()

	return sim.syncClusterResourceList(cluster)
}

func (sim *Simulator) ScheduleApp(ctx context.Context, app AppResource) (*SimulateResult, error) {
	// 由 AppResource 生成 Pods
	appPods, err := GenerateValidPodsFromAppResources(sim.fakeclient, app.Name, app.Resource)
	if err != nil {
		return nil, err
	}
	// affinityPriority := algo.NewAffinityQueue(appPods)
	// sort.Sort(affinityPriority)
	// tolerationPriority := algo.NewTolerationQueue(appPods)
	// sort.Sort(tolerationPriority)

	if sim.kubeclient != nil {
		for _, patchPods := range sim.patchPodFuncMap {
			if err := patchPods(appPods, sim.kubeclient); err != nil {
				return nil, err
			}
		}
	}

	for _, cm := range app.Resource.ConfigMaps {
		if _, err := sim.fakeclient.CoreV1().ConfigMaps(cm.Namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			return nil, err
		}
	}
	for _, sc := range app.Resource.StorageClasss {
		if _, err := sim.fakeclient.StorageV1().StorageClasses().Create(ctx, sc, metav1.CreateOptions{}); err != nil {
			return nil, err
		}
	}
	for _, pdb := range app.Resource.PodDisruptionBudgets {
		if _, err := sim.fakeclient.PolicyV1beta1().PodDisruptionBudgets(pdb.Namespace).Create(ctx, pdb, metav1.CreateOptions{}); err != nil {
			return nil, err
		}
	}

	failedPod, err := sim.schedulePods(ctx, appPods)
	if err != nil {
		return nil, err
	}
	return &SimulateResult{
		UnscheduledPods: failedPod,
		NodeStatus:      sim.getClusterNodeStatus(),
	}, nil
}

func (sim *Simulator) getClusterNodeStatus() []NodeStatus {
	var nodeStatues []NodeStatus
	nodeStatusMap := make(map[string]NodeStatus)
	nodes, _ := sim.fakeclient.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	allPods, _ := sim.fakeclient.CoreV1().Pods(corev1.NamespaceAll).List(context.Background(), metav1.ListOptions{})

	for _, node := range nodes.Items {
		nodeStatus := NodeStatus{}
		nodeStatus.Node = node.DeepCopy()
		nodeStatus.Pods = make([]*corev1.Pod, 0)
		nodeStatusMap[node.Name] = nodeStatus
	}

	for _, pod := range allPods.Items {
		nodeStatus := nodeStatusMap[pod.Spec.NodeName]
		nodeStatus.Pods = append(nodeStatus.Pods, pod.DeepCopy())
		nodeStatusMap[pod.Spec.NodeName] = nodeStatus
	}

	for _, node := range nodes.Items {
		status := nodeStatusMap[node.Name]
		nodeStatues = append(nodeStatues, status)
	}
	return nodeStatues
}

// runScheduler
func (sim *Simulator) runScheduler() {
	go sim.scheduler.Run(sim.scheduleOneCtx)
}

// Run starts to schedule pods
func (sim *Simulator) schedulePods(ctx context.Context, pods []*corev1.Pod) ([]UnscheduledPod, error) {
	var failedPods []UnscheduledPod
	var progressBar *pterm.ProgressbarPrinter
	if !sim.disablePTerm {
		progressBar, _ = pterm.DefaultProgressbar.WithTotal(len(pods)).Start()
		defer func() {
			_, _ = progressBar.Stop()
		}()
	}
	for _, pod := range pods {
		if !sim.disablePTerm {
			// Update the title of the progressbar.
			progressBar.UpdateTitle(fmt.Sprintf("%s/%s", pod.Namespace, pod.Name))
		}
		if _, err := sim.fakeclient.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("%s %s/%s: %s", simontype.CreatePodError, pod.Namespace, pod.Name, err.Error())
		}

		// we send value into sim.simulatorStop channel in update() function only,
		// update() is triggered when pod without nodename is handled.
		if pod.Spec.NodeName == "" {
			<-sim.simulatorStop
		}

		if strings.Contains(sim.status.stopReason, "failed") {
			if err := sim.fakeclient.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
				return nil, fmt.Errorf("%s %s/%s: %s", simontype.DeletePodError, pod.Namespace, pod.Name, err.Error())
			}
			failedPods = append(failedPods, UnscheduledPod{
				Pod:    pod,
				Reason: sim.status.stopReason,
			})
			sim.status.stopReason = ""
		}
		if !sim.disablePTerm {
			progressBar.Increment()
		}
	}
	return failedPods, nil
}

func (sim *Simulator) Close() {
	sim.scheduleOneCancelFunc()
	testpod := test.MakeFakePod("test", "test", "", "")
	_, err := sim.fakeclient.CoreV1().Pods("test").Create(context.TODO(), testpod, metav1.CreateOptions{})
	if err != nil {
		fmt.Printf("simon close with error: %s\n", err.Error())
	}
	if testpod.Spec.NodeName == "" {
		<-sim.simulatorStop
	}
	sim.cancelFunc()
	close(sim.simulatorStop)
	sim.eventBroadcaster.Shutdown()
}

func (sim *Simulator) syncClusterResourceList(resourceList ResourceTypes) (*SimulateResult, error) {
	//sync node
	for _, item := range resourceList.Nodes {
		if _, err := sim.fakeclient.CoreV1().Nodes().Create(context.TODO(), item, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("unable to copy node: %v", err)
		}
	}

	//sync pdb
	for _, item := range resourceList.PodDisruptionBudgets {
		if _, err := sim.fakeclient.PolicyV1beta1().PodDisruptionBudgets(item.Namespace).Create(context.TODO(), item, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("unable to copy PDB: %v", err)
		}
	}

	//sync svc
	for _, item := range resourceList.Services {
		if _, err := sim.fakeclient.CoreV1().Services(item.Namespace).Create(context.TODO(), item, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("unable to copy service: %v", err)
		}
	}

	//sync storage class
	for _, item := range resourceList.StorageClasss {
		if _, err := sim.fakeclient.StorageV1().StorageClasses().Create(context.TODO(), item, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("unable to copy storage class: %v", err)
		}
	}

	//sync pvc
	for _, item := range resourceList.PersistentVolumeClaims {
		if _, err := sim.fakeclient.CoreV1().PersistentVolumeClaims(item.Namespace).Create(context.TODO(), item, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("unable to copy pvc: %v", err)
		}
	}

	//sync deployment
	for _, item := range resourceList.Deployments {
		if _, err := sim.fakeclient.AppsV1().Deployments(item.Namespace).Create(context.TODO(), item, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("unable to copy deployment: %v", err)
		}
	}

	//sync rs
	for _, item := range resourceList.ReplicaSets {
		if _, err := sim.fakeclient.AppsV1().ReplicaSets(item.Namespace).Create(context.TODO(), item, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("unable to copy replica set: %v", err)
		}
	}

	//sync statefulset
	for _, item := range resourceList.StatefulSets {
		if _, err := sim.fakeclient.AppsV1().StatefulSets(item.Namespace).Create(context.TODO(), item, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("unable to copy stateful set: %v", err)
		}
	}

	//sync daemonset
	for _, item := range resourceList.DaemonSets {
		if _, err := sim.fakeclient.AppsV1().DaemonSets(item.Namespace).Create(context.TODO(), item, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("unable to copy daemon set: %v", err)
		}
	}

	// sync cm
	for _, item := range resourceList.ConfigMaps {
		if _, err := sim.fakeclient.CoreV1().ConfigMaps(item.Namespace).Create(context.TODO(), item, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("unable to copy configmap: %v", err)
		}
	}

	// sync pods
	failedPods, err := sim.schedulePods(context.TODO(), resourceList.Pods)
	if err != nil {
		return nil, err
	}

	return &SimulateResult{
		UnscheduledPods: failedPods,
		NodeStatus:      sim.getClusterNodeStatus(),
	}, nil
}

func (sim *Simulator) update(pod *corev1.Pod) {
	var stop bool = false
	var stopReason string
	var stopMessage string
	for _, podCondition := range pod.Status.Conditions {
		// log.Infof("podCondition %v", podCondition)
		stop = podCondition.Type == corev1.PodScheduled && podCondition.Status == corev1.ConditionFalse && podCondition.Reason == corev1.PodReasonUnschedulable
		if stop {
			stopReason = podCondition.Reason
			stopMessage = podCondition.Message
			// fmt.Printf("stop is true: %s %s\n", stopReason, stopMessage)
			break
		}
	}
	// Only for pending pods provisioned by simon
	if stop {
		sim.status.stopReason = fmt.Sprintf("failed to schedule pod (%s/%s): %s: %s", pod.Namespace, pod.Name, stopReason, stopMessage)
	}
	sim.simulatorStop <- struct{}{}
}

// WithKubeConfig sets kubeconfig for Simulator, the default value is ""
func WithKubeConfig(kubeconfig string) Option {
	return func(o *simulatorOptions) {
		o.kubeconfig = kubeconfig
	}
}

// WithSchedulerConfig sets schedulerConfig for Simulator, the default value is ""
func WithSchedulerConfig(schedulerConfig string) Option {
	return func(o *simulatorOptions) {
		o.schedulerConfig = schedulerConfig
	}
}

func WithExtraRegistry(extraRegistry frameworkruntime.Registry) Option {
	return func(o *simulatorOptions) {
		o.extraRegistry = extraRegistry
	}
}

func WithPatchPodsFuncMap(patchPodsFuncMap PatchPodsFuncMap) Option {
	return func(o *simulatorOptions) {
		o.patchPodFuncMap = patchPodsFuncMap
	}
}

func WithScheduleConfigOpts(scheduleConfigOpts []ScheduleConfigOption) Option {
	return func(o *simulatorOptions) {
		o.scheduleConfigOpts = scheduleConfigOpts
	}
}

func DisablePTerm(disablePTerm bool) Option {
	return func(o *simulatorOptions) {
		o.disablePTerm = disablePTerm
	}
}

// CreateClusterResourceFromClient returns a ResourceTypes struct by kube-client that connects a real cluster
func CreateClusterResourceFromClient(client externalclientset.Interface, disablePTerm bool) (ResourceTypes, error) {
	var resource ResourceTypes
	var err error
	var spinner *pterm.SpinnerPrinter
	if !disablePTerm {
		spinner, _ = pterm.DefaultSpinner.WithShowTimer().Start("get resource info from kube client")
	}

	trace := utiltrace.New("Trace CreateClusterResourceFromClient")
	defer trace.LogIfLong(100 * time.Millisecond)
	nodeItems, err := client.CoreV1().Nodes().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return resource, fmt.Errorf("unable to list nodes: %v", err)
	}
	for _, item := range nodeItems.Items {
		newItem := item
		resource.Nodes = append(resource.Nodes, &newItem)
	}
	trace.Step("CreateClusterResourceFromClient: List Node done")

	// We will regenerate pods of all workloads in the follow-up stage.
	podItems, err := client.CoreV1().Pods(metav1.NamespaceAll).List(context.TODO(), metav1.ListOptions{ResourceVersion: "0"})
	if err != nil {
		return resource, fmt.Errorf("unable to list pods: %v", err)
	}
	pendingPods := []*corev1.Pod{}
	for _, item := range podItems.Items {
		if !utils.OwnedByDaemonset(item.OwnerReferences) && item.DeletionTimestamp == nil {
			if item.Status.Phase == corev1.PodRunning {
				newItem := item
				resource.Pods = append(resource.Pods, &newItem)
			} else if item.Status.Phase == corev1.PodPending {
				newItem := item
				pendingPods = append(pendingPods, &newItem)
			}
		}
	}
	resource.Pods = append(resource.Pods, pendingPods...)
	trace.Step("CreateClusterResourceFromClient: List Pod done")

	pdbItems, err := client.PolicyV1beta1().PodDisruptionBudgets(metav1.NamespaceAll).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return resource, fmt.Errorf("unable to list PDBs: %v", err)
	}
	for _, item := range pdbItems.Items {
		newItem := item
		resource.PodDisruptionBudgets = append(resource.PodDisruptionBudgets, &newItem)
	}

	serviceItems, err := client.CoreV1().Services(metav1.NamespaceAll).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return resource, fmt.Errorf("unable to list services: %v", err)
	}
	for _, item := range serviceItems.Items {
		newItem := item
		resource.Services = append(resource.Services, &newItem)
	}

	storageClassesItems, err := client.StorageV1().StorageClasses().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return resource, fmt.Errorf("unable to list storage classes: %v", err)
	}
	for _, item := range storageClassesItems.Items {
		newItem := item
		resource.StorageClasss = append(resource.StorageClasss, &newItem)
	}

	pvcItems, err := client.CoreV1().PersistentVolumeClaims(metav1.NamespaceAll).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return resource, fmt.Errorf("unable to list pvcs: %v", err)
	}
	for _, item := range pvcItems.Items {
		newItem := item
		resource.PersistentVolumeClaims = append(resource.PersistentVolumeClaims, &newItem)
	}

	cmItems, err := client.CoreV1().ConfigMaps(metav1.NamespaceAll).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return resource, fmt.Errorf("unable to list configmaps: %v", err)
	}
	for _, item := range cmItems.Items {
		newItem := item
		resource.ConfigMaps = append(resource.ConfigMaps, &newItem)
	}

	daemonSetItems, err := client.AppsV1().DaemonSets(metav1.NamespaceAll).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return resource, fmt.Errorf("unable to list daemon sets: %v", err)
	}
	for _, item := range daemonSetItems.Items {
		newItem := item
		resource.DaemonSets = append(resource.DaemonSets, &newItem)
	}
	if !disablePTerm {
		spinner.Success("get resource info from kube client done!")
	}

	return resource, nil
}

// CreateClusterResourceFromClusterConfig return a ResourceTypes struct based on the cluster config
func CreateClusterResourceFromClusterConfig(path string) (ResourceTypes, error) {
	var resource ResourceTypes
	var content []string
	var err error

	if content, err = utils.GetYamlContentFromDirectory(path); err != nil {
		return ResourceTypes{}, fmt.Errorf("failed to get the yaml content from the cluster directory(%s): %v", path, err)
	}
	if resource, err = GetObjectFromYamlContent(content); err != nil {
		return resource, err
	}

	MatchAndSetLocalStorageAnnotationOnNode(resource.Nodes, path)

	return resource, nil
}
