package apply

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"strconv"

	survey "github.com/AlecAivazis/survey/v2"
	"github.com/pquerna/ffjson/ffjson"
	"github.com/pterm/pterm"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	resourcehelper "k8s.io/kubectl/pkg/util/resource"
	"sigs.k8s.io/yaml"

	"github.com/alibaba/open-simulator/pkg/api/v1alpha1"
	"github.com/alibaba/open-simulator/pkg/chart"
	"github.com/alibaba/open-simulator/pkg/simulator"
	simontype "github.com/alibaba/open-simulator/pkg/type"
	gpusharecache "github.com/alibaba/open-simulator/pkg/type/open-gpu-share/cache"
	gpushareutils "github.com/alibaba/open-simulator/pkg/type/open-gpu-share/utils"
	"github.com/alibaba/open-simulator/pkg/utils"
)

const (
	SurveyAddNode     = "add node(s)"
	SurveyShowResults = "show error event of unscheduled pods"
	SurveyExit        = "exit"
)

type Options struct {
	SimonConfig                string
	DefaultSchedulerConfigFile string
	UseGreed                   bool
	Interactive                bool
	ExtendedResources          []string
	OutputFile                 string
}

type Applier struct {
	cluster           v1alpha1.Cluster
	appList           []v1alpha1.AppInfo
	newNodePath       string
	schedulerConfig   string
	useGreed          bool
	interactive       bool
	extendedResources []string
	outputFile        *os.File
}

type Interface interface {
	Run() error
}

// NewApplier returns a default applier that has passed the validity test
func NewApplier(opts Options) Interface {
	simonCR := &v1alpha1.Simon{}
	configFile, err := ioutil.ReadFile(opts.SimonConfig)
	if err != nil {
		log.Fatalf("failed to read config file(%s): %v", opts.SimonConfig, err)
	}
	configJSON, err := yaml.YAMLToJSON(configFile)
	if err != nil {
		log.Fatalf("failed to unmarshal config file(%s) to json: %v", opts.SimonConfig, err)
	}

	if err := json.Unmarshal(configJSON, simonCR); err != nil {
		log.Fatalf("failed to unmarshal config json to object: %v", err)
	}

	var outputFile *os.File = nil
	if opts.OutputFile != "" {
		outputFile, err = os.Create(opts.OutputFile)
		if err != nil {
			log.Fatalf("failed to open file %s: %s", opts.OutputFile, err.Error())
		}
	}

	applier := &Applier{
		cluster:           simonCR.Spec.Cluster,
		appList:           simonCR.Spec.AppList,
		newNodePath:       simonCR.Spec.NewNode,
		schedulerConfig:   opts.DefaultSchedulerConfigFile,
		useGreed:          opts.UseGreed,
		interactive:       opts.Interactive,
		extendedResources: opts.ExtendedResources,
		outputFile:        outputFile,
	}

	if err := validate(applier); err != nil {
		pterm.FgRed.Printf("%v", err)
		os.Exit(1)
	}

	return applier
}

func (applier *Applier) Run() (err error) {
	// Step 0: init
	disablePTerm := false
	if applier.outputFile != nil {
		pterm.SetDefaultOutput(applier.outputFile)
		disablePTerm = true
	}

	// Step 1: convert the cluster files into the kubernetes objects and generate a ResourceTypes struct
	// cluster resource generated by two types of cluster, custom cluster and real cluster
	var clusterResourceCopy simulator.ResourceTypes
	if applier.cluster.KubeConfig != "" {
		// generate kube-client
		kubeclient, err := utils.CreateKubeClient(applier.cluster.KubeConfig)
		if err != nil {
			return err
		}
		if clusterResourceCopy, err = simulator.CreateClusterResourceFromClient(kubeclient, disablePTerm); err != nil {
			return err
		}
	} else {
		if clusterResourceCopy, err = simulator.CreateClusterResourceFromClusterConfig(applier.cluster.CustomCluster); err != nil {
			return err
		}
	}

	// Step 2: convert the application files into the kubernetes objects and generate a ResourceTypes struct, then make a resource list
	var appResource simulator.ResourceTypes
	var resourceList []string
	var content []string
	resourceMap := make(map[string]simulator.ResourceTypes)
	for _, app := range applier.appList {
		// process separately chart and other file
		if app.Chart {
			// parse and render chart as a yaml array
			if content, err = chart.ProcessChart(app.Name, app.Path); err != nil {
				return err
			}
		} else {
			if content, err = utils.GetYamlContentFromDirectory(app.Path); err != nil {
				return err
			}
		}
		if appResource, err = simulator.GetObjectFromYamlContent(content); err != nil {
			return err
		}

		resourceMap[app.Name] = appResource
		resourceList = append(resourceList, app.Name)
	}

	// Step 3: convert the path of the new node to be added into the kubernetes object
	// only support temporarily one type of node at present
	var nodeResource simulator.ResourceTypes
	var newNode *corev1.Node
	if applier.newNodePath != "" {
		if content, err = utils.GetYamlContentFromDirectory(applier.newNodePath); err != nil {
			return err
		}
		if nodeResource, err = simulator.GetObjectFromYamlContent(content); err != nil {
			return err
		}
		simulator.MatchAndSetLocalStorageAnnotationOnNode(nodeResource.Nodes, applier.newNodePath)
		// only support temporarily adding a type of node at present
		newNode = nodeResource.Nodes[0]
	}

	// confirm the list of applications that needed to be deployed in interactive mode
	var selectedAppNameList []string
	var selectedResourceList []simulator.AppResource
	if len(resourceList) != 0 && applier.interactive {
		var multiQs = []*survey.Question{
			{
				Name: "APPs",
				Prompt: &survey.MultiSelect{
					Message: "Confirm your apps :",
					Options: resourceList,
				},
			},
		}
		err = survey.Ask(multiQs, &selectedAppNameList)
		if err != nil {
			log.Fatalf("%v", err)
		}
	} else {
		selectedAppNameList = resourceList
	}
	for _, name := range selectedAppNameList {
		selectedResourceList = append(selectedResourceList, simulator.AppResource{
			Name:     name,
			Resource: resourceMap[name],
		})
	}

	// Step 4: determining that the current cluster can deploy selected applications and meets the given requests,
	// If everything is ok, output the result. Otherwise we adjust the scale of cluster by adding node
	var result *simulator.SimulateResult
	canBeScheduled := false
	newNodeNum := 0
	choose := ""
	for {
		if choose != SurveyShowResults {
			newClusterResource := clusterResourceCopy
			nodes, err := utils.NewFakeNodes(newNode, newNodeNum)
			if err != nil {
				return err
			}
			newClusterResource.Nodes = append(newClusterResource.Nodes, nodes...)
			result, err = simulator.Simulate(newClusterResource, selectedResourceList, simulator.DisablePTerm(disablePTerm), simulator.WithKubeConfig(applier.cluster.KubeConfig))

			if err != nil {
				return err
			}
		}

		if len(result.UnscheduledPods) != 0 {
			prompt := &survey.Select{
				Message: fmt.Sprintf("there are still %d pod(s) that can not be scheduled when add %d nodes, you can:", len(result.UnscheduledPods), newNodeNum),
				Options: []string{SurveyShowResults, SurveyAddNode, SurveyExit},
			}
			err = survey.AskOne(prompt, &choose)
			if err != nil {
				log.Fatalf("%v", err)
			}
			exitSimulation := false
			switch choose {
			case SurveyShowResults:
				for i, pod := range result.UnscheduledPods {
					pterm.FgLightWhite.Printf("%4d %s/%s: %s\n", i, pod.Pod.Namespace, pod.Pod.Name, pod.Reason)
				}
			case SurveyAddNode:
				num := 0
				prompt := &survey.Input{
					Message: "input node number",
				}
				err = survey.AskOne(prompt, &num)
				if err != nil {
					log.Fatalf("%v", err)
				}
				newNodeNum = num
			case SurveyExit:
				exitSimulation = true
			}
			if exitSimulation {
				break
			}
		} else {
			if ok, reason, err := satisfyResourceSetting(result.NodeStatus); err != nil {
				return err
			} else if !ok {
				pterm.FgLightWhite.Println(reason)
			} else {
				canBeScheduled = true
				break
			}
		}
	}

	if canBeScheduled {
		pterm.FgGreen.Println("Simulation success!")
		report(result.NodeStatus, applier.extendedResources, selectedAppNameList)
	}

	return nil
}

func validate(applier *Applier) error {
	if len(applier.cluster.KubeConfig) == 0 && len(applier.cluster.CustomCluster) == 0 ||
		len(applier.cluster.KubeConfig) != 0 && len(applier.cluster.CustomCluster) != 0 {
		return fmt.Errorf("only one of values of both kubeConfig and customConfig must exist ")
	}

	if len(applier.cluster.KubeConfig) != 0 {
		if _, err := os.Stat(applier.cluster.KubeConfig); err != nil {
			return fmt.Errorf("invalid path of kubeConfig: %v ", err)
		}
	}

	if len(applier.cluster.CustomCluster) != 0 {
		if _, err := os.Stat(applier.cluster.CustomCluster); err != nil {
			return fmt.Errorf("invalid path of customConfig: %v ", err)
		}
	}

	if len(applier.schedulerConfig) != 0 {
		if _, err := os.Stat(applier.schedulerConfig); err != nil {
			return fmt.Errorf("invalid path of scheduler config: %v ", err)
		}
	}

	if len(applier.newNodePath) != 0 {
		if _, err := os.Stat(applier.newNodePath); err != nil {
			return fmt.Errorf("invalid path of newNode: %v ", err)
		}
	}

	for _, app := range applier.appList {
		if _, err := os.Stat(app.Path); err != nil {
			return fmt.Errorf("invalid path of %s app: %v ", app.Name, err)
		}
	}

	return nil
}

// report print out scheduling result of pods
func report(nodeStatuses []simulator.NodeStatus, extendedResources []string, appNameList []string) {
	reportClusterInfo(nodeStatuses, extendedResources)
	reportNodeInfo(nodeStatuses, extendedResources)
	reportAppInfo(nodeStatuses, appNameList)
}

func reportClusterInfo(nodeStatuses []simulator.NodeStatus, extendedResources []string) {
	pterm.FgYellow.Println("Node Info")
	clusterTable := pterm.DefaultTable.WithHasHeader()
	var clusterTableData [][]string
	nodeTableHeader := []string{
		"Node",
		"CPU Allocatable",
		"CPU Requests",
		"Memory Allocatable",
		"Memory Requests",
	}
	if containGpu(extendedResources) {
		nodeTableHeader = append(nodeTableHeader, []string{
			"GPU Mem Allocatable",
			"GPU Mem Requests",
		}...)
	}
	nodeTableHeader = append(nodeTableHeader, []string{
		"Pod Count",
		"New Node",
	}...)
	clusterTableData = append(clusterTableData, nodeTableHeader)

	nodeReq := make(map[string]map[corev1.ResourceName]resource.Quantity, len(nodeStatuses))
	for _, status := range nodeStatuses {
		reqs := make(map[corev1.ResourceName]resource.Quantity)
		nodeReq[status.Node.Name] = reqs
		for _, pod := range status.Pods {
			podReqs, _ := resourcehelper.PodRequestsAndLimits(pod)
			for podReqName, podReqValue := range podReqs {
				if value, ok := reqs[podReqName]; !ok {
					reqs[podReqName] = podReqValue.DeepCopy()
				} else {
					value.Add(podReqValue)
					reqs[podReqName] = value
				}
			}
		}
	}
	for _, status := range nodeStatuses {
		node := status.Node
		allocatable := node.Status.Allocatable
		reqs := nodeReq[status.Node.Name]
		nodeCpuReq, nodeMemoryReq := reqs[corev1.ResourceCPU], reqs[corev1.ResourceMemory]
		nodeCpuReqFraction := float64(nodeCpuReq.MilliValue()) / float64(allocatable.Cpu().MilliValue()) * 100
		nodeMemoryReqFraction := float64(nodeMemoryReq.Value()) / float64(allocatable.Memory().Value()) * 100
		newNode := ""
		if _, exist := node.Labels[simontype.LabelNewNode]; exist {
			newNode = "√"
		}

		data := []string{
			node.Name,
			allocatable.Cpu().String(),
			fmt.Sprintf("%s(%d%%)", nodeCpuReq.String(), int64(nodeCpuReqFraction)),
			allocatable.Memory().String(),
			fmt.Sprintf("%s(%d%%)", nodeMemoryReq.String(), int64(nodeMemoryReqFraction)),
		}
		if containGpu(extendedResources) {
			nodeGpuMemReq := resource.NewQuantity(0, resource.BinarySI)
			for _, pod := range status.Pods {
				gpuMem, gpuNum := gpushareutils.GetGpuMemoryAndCountFromPodAnnotation(pod)
				gpuMemReq := resource.NewQuantity(int64(gpuMem*gpuNum), resource.BinarySI)
				nodeGpuMemReq.Add(*gpuMemReq)
			}
			nodeGpuMemFraction := float64(nodeGpuMemReq.Value()) / float64(allocatable.Name(gpushareutils.ResourceName, resource.BinarySI).Value()) * 100
			data = append(data, []string{
				allocatable.Name(gpushareutils.ResourceName, resource.BinarySI).String(),
				fmt.Sprintf("%s(%d%%)", nodeGpuMemReq.String(), int64(nodeGpuMemFraction)),
			}...)
		}
		data = append(data, []string{
			fmt.Sprintf("%d", len(status.Pods)),
			newNode,
		}...)
		clusterTableData = append(clusterTableData, data)
	}
	if err := clusterTable.WithData(clusterTableData).Render(); err != nil {
		pterm.FgRed.Printf("fail to render cluster table: %s\n", err.Error())
		os.Exit(1)
	}
	pterm.FgYellow.Println()

	// report extended resource info (e.g., node storage, GPU)
	if len(extendedResources) != 0 {
		pterm.FgYellow.Println("Extended Resource Info")
		if containGpu(extendedResources) {
			var podList []*corev1.Pod
			pterm.FgYellow.Println("GPU Node Resource")
			nodeGpuTable := pterm.DefaultTable.WithHasHeader()
			var nodeGpuTableData [][]string
			nodeGpuTableData = append(nodeGpuTableData, []string{"Node", "GPU ID", "GPU Request/Capacity", "Pod List"})
			for _, status := range nodeStatuses {
				node := status.Node
				podList = append(podList, status.Pods...)
				if nodeGpuInfoStr, exist := node.Annotations[simontype.AnnoNodeGpuShare]; exist {
					var nodeGpuInfo gpusharecache.NodeGpuInfo
					if err := ffjson.Unmarshal([]byte(nodeGpuInfoStr), &nodeGpuInfo); err != nil {
						log.Errorf("failed to unmarshal storage information of node(%s: %v", node.Name, err)
						continue
					}
					nodeGpuMemReq := resource.NewQuantity(0, resource.BinarySI)
					for _, pod := range status.Pods {
						gpuMem, gpuNum := gpushareutils.GetGpuMemoryAndCountFromPodAnnotation(pod)
						gpuMemReq := resource.NewQuantity(int64(gpuMem*gpuNum), resource.BinarySI)
						nodeGpuMemReq.Add(*gpuMemReq)
					}
					gpuReqCapFraction := float64(nodeGpuMemReq.Value()) / float64(nodeGpuInfo.GpuTotalMemory.Value()) * 100
					gpuReqCapStr := fmt.Sprintf("%s/%s(%d%%)", nodeGpuMemReq.String(), nodeGpuInfo.GpuTotalMemory.String(), int(gpuReqCapFraction))
					nodeOutputLine := []string{fmt.Sprintf("%s (%s)", node.Name, nodeGpuInfo.GpuModel), fmt.Sprintf("%d GPUs", nodeGpuInfo.GpuCount), gpuReqCapStr, fmt.Sprintf("%d Pods", nodeGpuInfo.NumPods)}
					nodeGpuTableData = append(nodeGpuTableData, nodeOutputLine)

					for idx := 0; idx < len(nodeGpuInfo.DevsBrief); idx += 1 {
						if deviceInfoBrief, ok := nodeGpuInfo.DevsBrief[idx]; ok {
							devTotalGpuMem := deviceInfoBrief.GpuTotalMemory
							if devTotalGpuMem.Value() <= 0 {
								continue // either no GPU or not allocated
							}
							devUsedGpuMem := deviceInfoBrief.GpuUsedMemory
							devReqCapFraction := float64(devUsedGpuMem.Value()) / float64(devTotalGpuMem.Value()) * 100
							devReqCapStr := fmt.Sprintf("%s/%s(%d%%)", devUsedGpuMem.String(), devTotalGpuMem.String(), int(devReqCapFraction))
							nodeOutputLineDev := []string{fmt.Sprintf("%s (%s)", node.Name, nodeGpuInfo.GpuModel), fmt.Sprintf("%d", idx), devReqCapStr, fmt.Sprintf("%s", deviceInfoBrief.PodList)}
							nodeGpuTableData = append(nodeGpuTableData, nodeOutputLineDev)
						}
					}
				}
			}
			// Send output
			if err := nodeGpuTable.WithData(nodeGpuTableData).Render(); err != nil {
				pterm.FgRed.Printf("fail to render cluster table: %s\n", err.Error())
				os.Exit(1)
			}

			pterm.FgYellow.Println("\nPod -> Node Map")
			podGpuTable := pterm.DefaultTable.WithHasHeader()
			var podGpuTableData [][]string
			podGpuTableData = append(podGpuTableData, []string{"Pod", "CPU Req", "Mem Req", "GPU Req", "Host Node", "GPU IDX"})
			sort.Slice(podList, func(i, j int) bool { return podList[i].Name < podList[j].Name })
			for _, pod := range podList {
				req, limit := resourcehelper.PodRequestsAndLimits(pod)
				gpuMem, gpuNum := gpushareutils.GetGpuMemoryAndCountFromPodAnnotation(pod)
				gpuMemReq := resource.NewQuantity(int64(gpuMem*gpuNum), resource.BinarySI)
				cpuReq, _, memoryReq, _ := req[corev1.ResourceCPU], limit[corev1.ResourceCPU], req[corev1.ResourceMemory], limit[corev1.ResourceMemory]
				gpuIndex := gpushareutils.GetGpuIdFromAnnotation(pod)
				podOutputLine := []string{pod.Name, cpuReq.String(), memoryReq.String(), gpuMemReq.String(), pod.Spec.NodeName, gpuIndex}
				podGpuTableData = append(podGpuTableData, podOutputLine)
			}
			// Send output
			if err := podGpuTable.WithData(podGpuTableData).Render(); err != nil {
				pterm.FgRed.Printf("fail to render cluster table: %s\n", err.Error())
				os.Exit(1)
			}
		}
	}
}

func reportNodeInfo(nodeStatuses []simulator.NodeStatus, extendedResources []string) {
	var nodeList []string
	var selectedNodeList []string
	selectedNodeMap := make(map[string]struct{})
	for _, node := range nodeStatuses {
		nodeList = append(nodeList, node.Node.Name)
	}
	var multiQs = []*survey.Question{
		{
			Name: "APPs",
			Prompt: &survey.MultiSelect{
				Message: "select nodes that you want to report:",
				Options: nodeList,
			},
		},
	}
	err := survey.Ask(multiQs, &selectedNodeList)
	if err != nil {
		log.Fatalf("%v", err)
	}
	for _, nodeName := range selectedNodeList {
		selectedNodeMap[nodeName] = struct{}{}
	}
	pterm.FgYellow.Println("Pod Info")
	header := []string{
		"Pod",
		"CPU Requests",
		"Memory Requests",
	}
	if containGpu(extendedResources) {
		header = append(header, "GPU Mem Requests")
	}
	header = append(header, "APP Name")

	for _, status := range nodeStatuses {
		node := status.Node
		if _, selected := selectedNodeMap[node.Name]; !selected {
			continue
		}
		pterm.FgYellow.Println(status.Node.Name)
		podTable := pterm.DefaultTable.WithHasHeader()
		var podTableData [][]string
		podTableData = append(podTableData, header)
		allocatable := node.Status.Allocatable
		for _, pod := range status.Pods {
			if pod.Spec.NodeName != node.Name {
				continue
			}
			req, limit := resourcehelper.PodRequestsAndLimits(pod)
			cpuReq, _, memoryReq, _ := req[corev1.ResourceCPU], limit[corev1.ResourceCPU], req[corev1.ResourceMemory], limit[corev1.ResourceMemory]
			fractionCpuReq := float64(cpuReq.MilliValue()) / float64(allocatable.Cpu().MilliValue()) * 100
			fractionMemoryReq := float64(memoryReq.Value()) / float64(allocatable.Memory().Value()) * 100

			// app name
			appname := ""
			if str, exist := pod.Labels[simontype.LabelAppName]; exist {
				appname = str
			}
			data := []string{
				fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
				fmt.Sprintf("%s(%d%%)", cpuReq.String(), int64(fractionCpuReq)),
				fmt.Sprintf("%s(%d%%)", memoryReq.String(), int64(fractionMemoryReq)),
			}

			// GPU
			if containGpu(extendedResources) {
				gpuMem, gpuNum := gpushareutils.GetGpuMemoryAndCountFromPodAnnotation(pod)
				gpuMemReq := resource.NewQuantity(int64(gpuMem*gpuNum), resource.BinarySI)
				fractionGpuMemReq := float64(gpuMemReq.Value()) / float64(allocatable.Name(gpushareutils.ResourceName, resource.BinarySI).Value()) * 100
				data = append(data, fmt.Sprintf("%s(%d%%)", gpuMemReq.String(), int64(fractionGpuMemReq)))
			}

			data = append(data, appname)
			podTableData = append(podTableData, data)
		}
		if err := podTable.WithData(podTableData).Render(); err != nil {
			pterm.FgRed.Printf("fail to render cluster table: %s\n", err.Error())
			os.Exit(1)
		}
		pterm.FgYellow.Println()
	}
}

func reportAppInfo(nodeStatuses []simulator.NodeStatus, appNameList []string) {
	var selectedAppNameList []string
	if len(appNameList) == 0 {
		return
	} else {
		var multiQs = []*survey.Question{
			{
				Name: "APPs",
				Prompt: &survey.MultiSelect{
					Message: "Select apps to show:",
					Options: appNameList,
				},
			},
		}
		err := survey.Ask(multiQs, &selectedAppNameList)
		if err != nil {
			log.Fatalf("%v", err)
		}

		pterm.FgYellow.Println("App Info")
		header := []string{
			"Pod",
			"App Name",
		}

		selectedAppNameMap := make(map[string]struct{}, len(selectedAppNameList))
		for _, name := range selectedAppNameList {
			selectedAppNameMap[name] = struct{}{}
		}

		for _, status := range nodeStatuses {
			podTable := pterm.DefaultTable.WithHasHeader()
			var podTableData [][]string
			podTableData = append(podTableData, header)
			for _, pod := range status.Pods {
				// app name
				appname := ""
				if str, exist := pod.Labels[simontype.LabelAppName]; exist {
					appname = str
				}
				if _, exist := selectedAppNameMap[appname]; !exist {
					continue
				}
				data := []string{
					fmt.Sprintf("%s/%s", pod.Namespace, pod.Name),
					appname,
				}
				podTableData = append(podTableData, data)
			}
			if len(podTableData) > 1 {
				pterm.FgYellow.Println(status.Node.Name)
				if err := podTable.WithData(podTableData).Render(); err != nil {
					pterm.FgRed.Printf("fail to render cluster table: %s\n", err.Error())
					os.Exit(1)
				}
				pterm.FgYellow.Println()
			}
		}
	}
}

func satisfyResourceSetting(nodeStatuses []simulator.NodeStatus) (bool, string, error) {
	var err error
	var maxcpu int = 100
	var maxmem int = 100
	var maxvg int = 100
	if str := os.Getenv(simontype.EnvMaxCPU); str != "" {
		if maxcpu, err = strconv.Atoi(str); err != nil {
			return false, "", fmt.Errorf("failed to convert env %s to int: %s ", simontype.EnvMaxCPU, err.Error())
		}
		if maxcpu > 100 || maxcpu < 0 {
			maxcpu = 100
		}
	}

	if str := os.Getenv(simontype.EnvMaxMemory); str != "" {
		if maxmem, err = strconv.Atoi(str); err != nil {
			return false, "", fmt.Errorf("failed to convert env %s to int: %s ", simontype.EnvMaxMemory, err.Error())
		}
		if maxmem > 100 || maxmem < 0 {
			maxmem = 100
		}
	}

	if str := os.Getenv(simontype.EnvMaxVG); str != "" {
		if maxvg, err = strconv.Atoi(str); err != nil {
			return false, "", fmt.Errorf("failed to convert env %s to int: %s ", simontype.EnvMaxVG, err.Error())
		}
		if maxvg > 100 || maxvg < 0 {
			maxvg = 100
		}
	}

	totalAllocatableResource := map[corev1.ResourceName]*resource.Quantity{
		corev1.ResourceCPU:    resource.NewQuantity(0, resource.DecimalSI),
		corev1.ResourceMemory: resource.NewQuantity(0, resource.DecimalSI),
	}
	totalUsedResource := map[corev1.ResourceName]*resource.Quantity{
		corev1.ResourceCPU:    resource.NewQuantity(0, resource.DecimalSI),
		corev1.ResourceMemory: resource.NewQuantity(0, resource.DecimalSI),
	}
	var allPods []corev1.Pod
	for _, status := range nodeStatuses {
		for _, pod := range status.Pods {
			allPods = append(allPods, *pod)
		}
	}

	for _, status := range nodeStatuses {
		node := status.Node
		totalAllocatableResource[corev1.ResourceCPU].Add(*node.Status.Allocatable.Cpu())
		totalAllocatableResource[corev1.ResourceMemory].Add(*node.Status.Allocatable.Memory())

		reqs, _ := utils.GetPodsTotalRequestsAndLimitsByNodeName(allPods, node.Name)
		totalUsedResource[corev1.ResourceCPU].Add(reqs[corev1.ResourceCPU])
		totalUsedResource[corev1.ResourceMemory].Add(reqs[corev1.ResourceMemory])
	}

	cpuOccupancyRate := int(float64(totalUsedResource[corev1.ResourceCPU].MilliValue()) / float64(totalAllocatableResource[corev1.ResourceCPU].MilliValue()) * 100)
	memoryOccupancyRate := int(float64(totalUsedResource[corev1.ResourceMemory].MilliValue()) / float64(totalAllocatableResource[corev1.ResourceMemory].MilliValue()) * 100)
	if cpuOccupancyRate > maxcpu {
		return false, fmt.Sprintf("the average occupancy rate(%d%%) of cpu goes beyond the env setting(%d%%)\n", cpuOccupancyRate, maxcpu), nil
	}
	if memoryOccupancyRate > maxmem {
		return false, fmt.Sprintf("the average occupancy rate(%d%%) of memory goes beyond the env setting(%d%%)\n", memoryOccupancyRate, maxmem), nil
	}

	return true, "", nil
}

func containGpu(extendedResources []string) bool {
	for _, res := range extendedResources {
		if res == "gpu" {
			return true
		}
	}
	return false
}
