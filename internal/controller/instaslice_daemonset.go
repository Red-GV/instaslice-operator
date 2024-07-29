/*
Copyright 2024.

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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"

	inferencev1alpha1 "codeflare.dev/instaslice/api/v1alpha1"
	"github.com/NVIDIA/go-nvml/pkg/nvml"
	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	nvdevice "github.com/NVIDIA/go-nvlib/pkg/nvlib/device"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// InstaSliceDaemonsetReconciler reconciles a InstaSliceDaemonset object
type InstaSliceDaemonsetReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	kubeClient *kubernetes.Clientset
	NodeName   string
}

//+kubebuilder:rbac:groups=inference.codeflare.dev,resources=instaslices,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=inference.codeflare.dev,resources=instaslices/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=inference.codeflare.dev,resources=instaslices/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=nodes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

var discoveredGpusOnHost []string

// Additional handler used for making NVML calls.
type deviceHandler struct {
	nvdevice nvdevice.Interface
	nvml     nvml.Interface
}

type MigProfile struct {
	C              int
	G              int
	GB             int
	GIProfileID    int
	CIProfileID    int
	CIEngProfileID int
}

type ResPatchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value string `json:"value"`
}

const (
	// AttributeMediaExtensions holds the string representation for the media extension MIG profile attribute.
	AttributeMediaExtensions = "me"
)

func (r *InstaSliceDaemonsetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = log.FromContext(ctx)
	logger := log.Log.WithName("InstaSlice-daemonset")

	var instasliceList inferencev1alpha1.InstasliceList

	if err := r.List(ctx, &instasliceList, &client.ListOptions{}); err != nil {
		logger.Error(err, "Error listing Instaslice")
	}
	for _, instaslice := range instasliceList.Items {
		nodeName := os.Getenv("NODE_NAME")
		if instaslice.Name == nodeName {
			for _, allocations := range instaslice.Spec.Allocations {
				if allocations.Allocationstatus == "creating" {
					//Assume pod only has one container with one GPU request
					var profileName string
					var Giprofileid int
					var Ciprofileid int
					var CiEngProfileid int
					var deviceUUID string
					var migUUID string
					var deviceForMig string
					var instasliceList inferencev1alpha1.InstasliceList
					var giId uint32
					var ciId uint32
					var podUUID = allocations.PodUUID
					ret := nvml.Init()
					if ret != nvml.SUCCESS {
						fmt.Printf("Unable to initialize NVML: %v \n", nvml.ErrorString(ret))
					}

					availableGpus, ret := nvml.DeviceGetCount()
					if ret != nvml.SUCCESS {
						fmt.Printf("Unable to get device count: %v \n", nvml.ErrorString(ret))
					}

					deviceForMig, profileName, Giprofileid, Ciprofileid, CiEngProfileid = r.getAllocation(ctx, instasliceList, deviceForMig, profileName, Giprofileid, Ciprofileid, CiEngProfileid)
					placement := nvml.GpuInstancePlacement{}
					for i := 0; i < availableGpus; i++ {
						device, ret := nvml.DeviceGetHandleByIndex(i)
						if ret != nvml.SUCCESS {
							fmt.Printf("Unable to get device at index %d: %v \n", i, nvml.ErrorString(ret))
						}

						uuid, ret := device.GetUUID()
						if ret != nvml.SUCCESS {
							fmt.Printf("Unable to get uuid of device at index %d: %v \n", i, nvml.ErrorString(ret))
						}
						if deviceForMig != uuid {
							continue
						}
						deviceUUID = uuid
						gpuPlacement := allocations.GPUUUID

						//Move to next GPU as this is not the selected GPU by the controller.

						if gpuPlacement != uuid {
							continue
						}

						device, retCodeForDevice := nvml.DeviceGetHandleByUUID(uuid)

						if retCodeForDevice != nvml.SUCCESS {
							fmt.Printf("error getting GPU device handle: %v \n", ret)
						}

						giProfileInfo, retCodeForGi := device.GetGpuInstanceProfileInfo(Giprofileid)
						if retCodeForGi != nvml.SUCCESS {
							logger.Error(retCodeForGi, "error getting GPU instance profile info", "giProfileInfo", giProfileInfo, "retCodeForGi", retCodeForGi)
						}

						logger.Info("The profile id is", "giProfileInfo", giProfileInfo.Id, "Memory", giProfileInfo.MemorySizeMB)

						// Path to the file containing the node name
						updatedPlacement := r.getAllocationsToprepare(ctx, placement)
						gi, retCodeForGiWithPlacement := device.CreateGpuInstanceWithPlacement(&giProfileInfo, &updatedPlacement)
						if retCodeForGiWithPlacement != nvml.SUCCESS {
							fmt.Printf("error creating GPU instance for '%v': %v \n ", &giProfileInfo, retCodeForGiWithPlacement)
						}
						giInfo, retForGiInfor := gi.GetInfo()
						if retForGiInfor != nvml.SUCCESS {
							fmt.Printf("error getting GPU instance info for '%v': %v \n", &giProfileInfo, retForGiInfor)
						}
						//TODO: figure out the compute slice scenario, I think Kubernetes does not support this use case yet
						ciProfileInfo, retCodeForCiProfile := gi.GetComputeInstanceProfileInfo(Ciprofileid, CiEngProfileid)
						if retCodeForCiProfile != nvml.SUCCESS {
							fmt.Printf("error getting Compute instance profile info for '%v': %v \n", ciProfileInfo, retCodeForCiProfile)
						}
						ci, retCodeForComputeInstance := gi.CreateComputeInstance(&ciProfileInfo)
						if retCodeForComputeInstance != nvml.SUCCESS {
							fmt.Printf("error creating Compute instance for '%v': %v \n", ci, retCodeForComputeInstance)
						}
						//get created mig details
						giId, migUUID, ciId = r.getCreatedSliceDetails(giInfo, ret, device, uuid, profileName, migUUID, ciId)
						//create slice only on one GPU, both CI and GI creation are succeeded.
						if retCodeForCiProfile == retCodeForGi {
							break
						}

					}
					nodeName := os.Getenv("NODE_NAME")
					failure, _, errorUpdatingCapacity := r.updateNodeCapacity(ctx, nodeName)
					if failure {
						logger.Error(errorUpdatingCapacity, "unable to update node capacity")
					}

					typeNamespacedName := types.NamespacedName{
						Name:      nodeName,
						Namespace: "default",
					}
					instaslice := &inferencev1alpha1.Instaslice{}
					errGettingobj := r.Get(ctx, typeNamespacedName, instaslice)

					if errGettingobj != nil {
						fmt.Printf("Error getting instaslice obj %v", errGettingobj)
					}
					existingAllocations := instaslice.Spec.Allocations[podUUID]
					existingAllocations.Allocationstatus = "created"
					instaslice.Spec.Allocations[podUUID] = existingAllocations
					r.createConfigMap(ctx, migUUID, existingAllocations.Namespace, existingAllocations.PodName)
					node := &v1.Node{}
					if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
						logger.Error(err, "unable to fetch Node")
					}
					patchData, err := createPatchData("org.instaslice/"+allocations.PodName, "1")
					if err != nil {
						logger.Error(err, "unable to create patch data")
					}
					// Apply the patch to add the resource
					if err := r.Status().Patch(ctx, node, client.RawPatch(types.JSONPatchType, patchData)); err != nil {
						logger.Error(err, "unable to patch Node status")
					}

					r.createPreparedEntry(ctx, profileName, podUUID, deviceUUID, giId, ciId, instaslice, migUUID)

				}

				if allocations.Allocationstatus == "deleted" {
					logger.Info("Performing cleanup ", "pod", allocations.PodName)

					deletePrepared := r.cleanUp(ctx, allocations.PodUUID, logger, nodeName)
					delete(instaslice.Spec.Allocations, allocations.PodUUID)
					delete(instaslice.Spec.Prepared, deletePrepared)
					errUpdatingAllocation := r.Update(ctx, &instaslice)
					if errUpdatingAllocation != nil {
						logger.Error(errUpdatingAllocation, "Error updating InstasSlice object")
					}
				}

			}
		}

	}

	return ctrl.Result{}, nil
}

func (r *InstaSliceDaemonsetReconciler) getAllocationsToprepare(ctx context.Context, placement nvml.GpuInstancePlacement) nvml.GpuInstancePlacement {
	var instasliceList inferencev1alpha1.InstasliceList
	if err := r.List(ctx, &instasliceList, &client.ListOptions{}); err != nil {
		fmt.Printf("Error listing Instaslice %v", err)
	}
	for _, instaslice := range instasliceList.Items {

		nodeName := os.Getenv("NODE_NAME")
		if instaslice.Name == nodeName {
			for _, v := range instaslice.Spec.Allocations {
				if v.Allocationstatus == "creating" {
					placement.Size = v.Size
					placement.Start = v.Start
				}
			}
		}
	}
	return placement
}

func (*InstaSliceDaemonsetReconciler) getCreatedSliceDetails(giInfo nvml.GpuInstanceInfo, ret nvml.Return, device nvml.Device, uuid string, profileName string, migUUID string, ciId uint32) (uint32, string, uint32) {
	giId := giInfo.Id
	h := &deviceHandler{}
	h.nvml = nvml.New()
	h.nvdevice = nvdevice.New(nvdevice.WithNvml(h.nvml))

	ret1 := h.nvml.Init()
	if ret1 != nvml.SUCCESS {
		fmt.Printf("Unable to initialize NVML: %v", nvml.ErrorString(ret))
	}
	nvlibParentDevice, err := h.nvdevice.NewDevice(device)
	if err != nil {
		fmt.Printf("unable to get nvlib GPU parent device for MIG UUID '%v': %v", uuid, ret)
	}
	migs, err := nvlibParentDevice.GetMigDevices()
	if err != nil {
		fmt.Printf("unable to get MIG devices on GPU '%v': %v", uuid, err)
	}
	for _, mig := range migs {
		obtainedProfileName, _ := mig.GetProfile()
		fmt.Printf("obtained profile is %v\n", obtainedProfileName)
		giID, ret := mig.GetGpuInstanceId()
		if ret != nvml.SUCCESS {
			fmt.Printf("error getting GPU instance ID for MIG device: %v", ret)
		}
		gpuInstance, err1 := device.GetGpuInstanceById(giID)
		if err1 != nvml.SUCCESS {
			fmt.Printf("Unable to get GPU instance %v\n", err1)
		}
		gpuInstanceInfo, err2 := gpuInstance.GetInfo()
		if err2 != nvml.SUCCESS {
			fmt.Printf("Unable to get GPU instance info %v\n", err2)
		}
		fmt.Printf("The instance info size %v and start %v\n", gpuInstanceInfo.Placement.Size, gpuInstanceInfo.Placement.Start)

		if profileName == obtainedProfileName.String() {
			realizedMig, _ := mig.GetUUID()
			migUUID = realizedMig
			migCid, _ := mig.GetComputeInstanceId()
			ci, _ := gpuInstance.GetComputeInstanceById(migCid)
			ciMigInfo, _ := ci.GetInfo()
			ciId = ciMigInfo.Id

		}
	}
	return giId, migUUID, ciId
}

func (r *InstaSliceDaemonsetReconciler) getAllocation(ctx context.Context, instasliceList inferencev1alpha1.InstasliceList, deviceForMig string, profileName string, Giprofileid int, Ciprofileid int, CiEngProfileid int) (string, string, int, int, int) {
	if err := r.List(ctx, &instasliceList, &client.ListOptions{}); err != nil {
		fmt.Printf("Error listing Instaslice %v", err)
	}
	for _, instaslice := range instasliceList.Items {
		nodeName := os.Getenv("NODE_NAME")
		if instaslice.Name == nodeName {
			for _, v := range instaslice.Spec.Allocations {
				if v.Allocationstatus == "creating" {
					deviceForMig = v.GPUUUID
					profileName = v.Profile
					Giprofileid = v.Giprofileid
					Ciprofileid = v.CIProfileID
					CiEngProfileid = v.CIEngProfileID
				}
			}
		}
	}
	return deviceForMig, profileName, Giprofileid, Ciprofileid, CiEngProfileid
}

func (r *InstaSliceDaemonsetReconciler) cleanUp(ctx context.Context, podUuid string, logger logr.Logger, nodeName string) string {
	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		logger.Error(ret, "Unable to initialize NVML")
	}
	var instasliceList inferencev1alpha1.InstasliceList
	if err := r.List(ctx, &instasliceList, &client.ListOptions{}); err != nil {
		fmt.Printf("Error listing Instaslice %v", err)
	}
	var candidateDel string
	for _, instaslice := range instasliceList.Items {
		nodeName := os.Getenv("NODE_NAME")
		if instaslice.Name == nodeName {
			prepared := instaslice.Spec.Prepared
			for migUUID, value := range prepared {
				if value.PodUUID == podUuid {
					parent, errRecievingDeviceHandle := nvml.DeviceGetHandleByUUID(value.Parent)
					if errRecievingDeviceHandle != nvml.SUCCESS {
						logger.Error(errRecievingDeviceHandle, "Error obtaining GPU handle")
					}
					gi, errRetrievingGi := parent.GetGpuInstanceById(int(value.Giinfoid))
					if errRetrievingGi != nvml.SUCCESS {
						logger.Error(errRetrievingGi, "Error obtaining GPU instance")
					}
					ci, errRetrievingCi := gi.GetComputeInstanceById(int(value.Ciinfoid))
					if errRetrievingCi != nvml.SUCCESS {
						logger.Error(errRetrievingCi, "Error obtaining Compute instance")
					}
					errDestroyingCi := ci.Destroy()
					if errDestroyingCi != nvml.SUCCESS {
						logger.Error(errDestroyingCi, "Error deleting Compute instance")
					}
					errDestroyingGi := gi.Destroy()
					if errDestroyingGi != nvml.SUCCESS {
						logger.Error(errDestroyingGi, "Error deleting GPU instance")
					}
					candidateDel = migUUID
					logger.Info("Done deleting MIG slice for pod", "UUID", value.PodUUID)
				}
			}

			for _, allocation := range instaslice.Spec.Allocations {
				if allocation.PodUUID == podUuid {
					deleteConfigMap(ctx, r.Client, allocation.PodName, allocation.Namespace)
					deletePatch, err := deletePatchData(allocation.PodName)
					if err != nil {
						logger.Error(err, "unable to create delete patch data")
						//return err
					}

					// Apply the patch to remove the resource
					node := &v1.Node{}
					if err := r.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
						logger.Error(err, "unable to fetch Node")
						//return err
					}
					if err := r.Status().Patch(ctx, node, client.RawPatch(types.JSONPatchType, deletePatch)); err != nil {
						logger.Error(err, "unable to patch Node status")
						//return err
					}

					logger.Info("Successfully patched Node status")

					r.updateNodeCapacity(ctx, nodeName)

				}
			}
		}
	}
	return candidateDel
}

func (r *InstaSliceDaemonsetReconciler) createPreparedEntry(ctx context.Context, profileName string, podUUID string, deviceUUID string, giId uint32, ciId uint32, instaslice *inferencev1alpha1.Instaslice, migUUID string) {
	updatedAllocation := instaslice.Spec.Allocations[podUUID]
	instaslicePrepared := inferencev1alpha1.PreparedDetails{
		Profile:  profileName,
		Start:    updatedAllocation.Start,
		Size:     updatedAllocation.Size,
		Parent:   deviceUUID,
		PodUUID:  podUUID,
		Giinfoid: giId,
		Ciinfoid: ciId,
	}
	if instaslice.Spec.Prepared == nil {
		instaslice.Spec.Prepared = make(map[string]inferencev1alpha1.PreparedDetails)
	}

	instaslice.Spec.Prepared[migUUID] = instaslicePrepared

	errForUpdate := r.Update(ctx, instaslice)

	if errForUpdate != nil {
		fmt.Printf("Error updating object %v", errForUpdate)
	}
}

// Reloads the configuration in the device plugin to update node capacity
func (r *InstaSliceDaemonsetReconciler) updateNodeCapacity(ctx context.Context, nodeName string) (bool, reconcile.Result, error) {
	node := &v1.Node{}
	nodeNameObject := types.NamespacedName{Name: nodeName}
	err := r.Get(ctx, nodeNameObject, node)
	if err != nil {
		fmt.Println("unable to fetch NodeLabeler, cannot update capacity")
	}
	// Check and update the label
	//TODO: change label name
	updated := false
	if value, exists := node.Labels["nvidia.com/device-plugin.config"]; exists && value == "a100-40gb-1" {
		node.Labels["nvidia.com/device-plugin.config"] = "a100-40gb"
		updated = true
	}
	if !updated {
		if value, exists := node.Labels["nvidia.com/device-plugin.config"]; exists && value == "a100-40gb" {
			node.Labels["nvidia.com/device-plugin.config"] = "a100-40gb-1"
			updated = true
		}
	}

	if updated {
		err = r.Update(ctx, node)
		if err != nil {
			fmt.Println("unable to update Node")
		}
	}
	return false, reconcile.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *InstaSliceDaemonsetReconciler) SetupWithManager(mgr ctrl.Manager) error {

	restConfig := mgr.GetConfig()

	var err error
	r.kubeClient, err = kubernetes.NewForConfig(restConfig)
	if err != nil {
		return err
	}
	if err := r.setupWithManager(mgr); err != nil {
		return err
	}

	//make InstaSlice object when it does not exists
	//if it got restarted then use the existing state.
	nodeName := os.Getenv("NODE_NAME")

	//Init InstaSlice obj as the first thing when cache is loaded.
	//RunnableFunc is added to the manager.
	//This function waits for the manager to be elected (<-mgr.Elected()) and then runs InstaSlice init code.
	mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		<-mgr.Elected() // Wait for the manager to be elected
		var instaslice inferencev1alpha1.Instaslice
		typeNamespacedName := types.NamespacedName{
			Name: nodeName,
			//TODO: change namespace
			Namespace: "default",
		}
		errRetrievingInstaSliceForSetup := r.Get(ctx, typeNamespacedName, &instaslice)
		if errRetrievingInstaSliceForSetup != nil {
			fmt.Printf("unable to fetch InstaSlice resource for node name %v which has error %v\n", nodeName, errRetrievingInstaSliceForSetup)
			//TODO: should we do hard exit?
			//os.Exit(1)
		}
		if instaslice.Status.Processed != "true" || (instaslice.Name == "" && instaslice.Namespace == "") {
			_, errForDiscoveringGpus := r.discoverMigEnabledGpuWithSlices()
			if errForDiscoveringGpus != nil {
				fmt.Printf("Error %v", errForDiscoveringGpus)
			}
		}
		return nil
	}))

	return nil
}

// Enable creation of controller caches to talk to the API server in order to perform
// object discovery in SetupWithManager
func (r *InstaSliceDaemonsetReconciler) setupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&inferencev1alpha1.Instaslice{}).Named("InstaSliceDaemonSet").
		Complete(r)
}

// This function discovers MIG devices as the plugin comes up. this is run exactly once.
func (r *InstaSliceDaemonsetReconciler) discoverMigEnabledGpuWithSlices() ([]string, error) {
	instaslice, _, gpuModelMap, failed, returnValue, errorDiscoveringProfiles := r.discoverAvailableProfilesOnGpus()
	if failed {
		return returnValue, errorDiscoveringProfiles
	}

	err := r.discoverDanglingSlices(instaslice)

	if err != nil {
		return nil, err
	}

	nodeName := os.Getenv("NODE_NAME")
	instaslice.Name = nodeName
	instaslice.Namespace = "default"
	instaslice.Spec.MigGPUUUID = gpuModelMap
	instaslice.Status.Processed = "true"
	//TODO: should we use context.TODO() ?
	customCtx := context.TODO()
	errToCreate := r.Create(customCtx, instaslice)
	if errToCreate != nil {
		return nil, errToCreate
	}

	// Object exists, update its status
	instaslice.Status.Processed = "true"
	if errForStatus := r.Status().Update(customCtx, instaslice); errForStatus != nil {
		return nil, errForStatus
	}

	return discoveredGpusOnHost, nil
}

func (r *InstaSliceDaemonsetReconciler) discoverAvailableProfilesOnGpus() (*inferencev1alpha1.Instaslice, nvml.Return, map[string]string, bool, []string, error) {
	instaslice := &inferencev1alpha1.Instaslice{}
	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		return nil, ret, nil, false, nil, ret
	}

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return nil, ret, nil, false, nil, ret
	}
	gpuModelMap := make(map[string]string)
	discoverProfilePerNode := true
	for i := 0; i < count; i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			return nil, ret, nil, false, nil, ret
		}

		uuid, _ := device.GetUUID()
		gpuName, _ := device.GetName()
		gpuModelMap[uuid] = gpuName
		discoveredGpusOnHost = append(discoveredGpusOnHost, uuid)
		if discoverProfilePerNode {

			for i := 0; i < nvml.GPU_INSTANCE_PROFILE_COUNT; i++ {
				giProfileInfo, ret := device.GetGpuInstanceProfileInfo(i)
				if ret == nvml.ERROR_NOT_SUPPORTED {
					continue
				}
				if ret == nvml.ERROR_INVALID_ARGUMENT {
					continue
				}
				if ret != nvml.SUCCESS {
					return nil, ret, nil, false, nil, ret
				}

				memory, ret := device.GetMemoryInfo()
				if ret != nvml.SUCCESS {
					return nil, ret, nil, false, nil, ret
				}

				profile := NewMigProfile(i, i, nvml.COMPUTE_INSTANCE_ENGINE_PROFILE_SHARED, giProfileInfo.SliceCount, giProfileInfo.SliceCount, giProfileInfo.MemorySizeMB, memory.Total)

				giPossiblePlacements, ret := device.GetGpuInstancePossiblePlacements(&giProfileInfo)
				if ret == nvml.ERROR_NOT_SUPPORTED {
					continue
				}
				if ret == nvml.ERROR_INVALID_ARGUMENT {
					continue
				}
				if ret != nvml.SUCCESS {
					return nil, 0, nil, true, nil, ret
				}
				placementsForProfile := []inferencev1alpha1.Placement{}
				for _, p := range giPossiblePlacements {
					placement := inferencev1alpha1.Placement{
						Size:  int(p.Size),
						Start: int(p.Start),
					}
					placementsForProfile = append(placementsForProfile, placement)
				}

				aggregatedPlacementsForProfile := inferencev1alpha1.Mig{
					Placements:     placementsForProfile,
					Profile:        profile.String(),
					Giprofileid:    i,
					CIProfileID:    profile.CIProfileID,
					CIEngProfileID: profile.CIEngProfileID,
				}
				instaslice.Spec.Migplacement = append(instaslice.Spec.Migplacement, aggregatedPlacementsForProfile)
			}
			discoverProfilePerNode = false
		}
	}
	return instaslice, ret, gpuModelMap, false, nil, nil
}

func (r *InstaSliceDaemonsetReconciler) discoverDanglingSlices(instaslice *inferencev1alpha1.Instaslice) error {
	h := &deviceHandler{}
	h.nvml = nvml.New()
	h.nvdevice = nvdevice.New(nvdevice.WithNvml(h.nvml))

	errInitNvml := h.nvml.Init()
	if errInitNvml != nvml.SUCCESS {
		return errInitNvml
	}

	availableGpusOnNode, errObtainingDeviceCount := h.nvml.DeviceGetCount()
	if errObtainingDeviceCount != nvml.SUCCESS {
		return errObtainingDeviceCount
	}

	for i := 0; i < availableGpusOnNode; i++ {
		device, errObtainingDeviceHandle := h.nvml.DeviceGetHandleByIndex(i)
		if errObtainingDeviceHandle != nvml.SUCCESS {
			return errObtainingDeviceHandle
		}

		uuid, errObtainingDeviceUUID := device.GetUUID()
		if errObtainingDeviceUUID != nvml.SUCCESS {
			return errObtainingDeviceUUID
		}

		nvlibParentDevice, errObtainingParentDevice := h.nvdevice.NewDevice(device)
		if errObtainingParentDevice != nil {
			return errObtainingParentDevice
		}
		migs, errRetrievingMigDevices := nvlibParentDevice.GetMigDevices()
		if errRetrievingMigDevices != nil {
			return errRetrievingMigDevices
		}

		for _, mig := range migs {
			migUUID, _ := mig.GetUUID()
			profile, errForProfile := mig.GetProfile()
			if errForProfile != nil {
				return errForProfile
			}

			giID, errForMigGid := mig.GetGpuInstanceId()
			if errForMigGid != nvml.SUCCESS {
				return errForMigGid
			}
			gpuInstance, errRetrievingDeviceGid := device.GetGpuInstanceById(giID)
			if errRetrievingDeviceGid != nvml.SUCCESS {
				return errRetrievingDeviceGid
			}
			gpuInstanceInfo, errObtainingInfo := gpuInstance.GetInfo()
			if errObtainingInfo != nvml.SUCCESS {
				return errObtainingInfo
			}

			ciID, ret := mig.GetComputeInstanceId()
			if ret != nvml.SUCCESS {
				return ret
			}
			ci, ret := gpuInstance.GetComputeInstanceById(ciID)
			if ret != nvml.SUCCESS {
				return ret
			}
			ciInfo, ret := ci.GetInfo()
			if ret != nvml.SUCCESS {
				return ret
			}
			prepared := inferencev1alpha1.PreparedDetails{
				Profile:  profile.GetInfo().String(),
				Start:    gpuInstanceInfo.Placement.Start,
				Size:     gpuInstanceInfo.Placement.Size,
				Parent:   uuid,
				Giinfoid: gpuInstanceInfo.Id,
				Ciinfoid: ciInfo.Id,
			}
			if instaslice.Spec.Prepared == nil {
				instaslice.Spec.Prepared = make(map[string]inferencev1alpha1.PreparedDetails)
			}
			instaslice.Spec.Prepared[migUUID] = prepared
		}
	}
	return nil
}

// NewMigProfile constructs a new MigProfile struct using info from the giProfiles and ciProfiles used to create it.
func NewMigProfile(giProfileID, ciProfileID, ciEngProfileID int, giSliceCount, ciSliceCount uint32, migMemorySizeMB, totalDeviceMemoryBytes uint64) *MigProfile {
	return &MigProfile{
		C:              int(ciSliceCount),
		G:              int(giSliceCount),
		GB:             int(getMigMemorySizeInGB(totalDeviceMemoryBytes, migMemorySizeMB)),
		GIProfileID:    giProfileID,
		CIProfileID:    ciProfileID,
		CIEngProfileID: ciEngProfileID,
	}
}

// Helper function to get GPU memory size in GBs.
func getMigMemorySizeInGB(totalDeviceMemory, migMemorySizeMB uint64) uint64 {
	const fracDenominator = 8
	const oneMB = 1024 * 1024
	const oneGB = 1024 * 1024 * 1024
	fractionalGpuMem := (float64(migMemorySizeMB) * oneMB) / float64(totalDeviceMemory)
	fractionalGpuMem = math.Ceil(fractionalGpuMem*fracDenominator) / fracDenominator
	totalMemGB := float64((totalDeviceMemory + oneGB - 1) / oneGB)
	return uint64(math.Round(fractionalGpuMem * totalMemGB))
}

// String returns the string representation of a MigProfile.
func (m MigProfile) String() string {
	var suffix string
	if len(m.Attributes()) > 0 {
		suffix = "+" + strings.Join(m.Attributes(), ",")
	}
	if m.C == m.G {
		return fmt.Sprintf("%dg.%dgb%s", m.G, m.GB, suffix)
	}
	return fmt.Sprintf("%dc.%dg.%dgb%s", m.C, m.G, m.GB, suffix)
}

// Attributes returns the list of attributes associated with a MigProfile.
func (m MigProfile) Attributes() []string {
	var attr []string
	switch m.GIProfileID {
	case nvml.GPU_INSTANCE_PROFILE_1_SLICE_REV1:
		attr = append(attr, AttributeMediaExtensions)
	}
	return attr
}

// Create configmap which is used by Pods to consume MIG device
func (r *InstaSliceDaemonsetReconciler) createConfigMap(ctx context.Context, migGPUUUID string, namespace string, podName string) {
	configMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
		},
		Data: map[string]string{
			"NVIDIA_VISIBLE_DEVICES": migGPUUUID,
			"CUDA_VISIBLE_DEVICES":   migGPUUUID,
		},
	}
	if err := r.Create(ctx, configMap); err != nil {
		log.FromContext(ctx).Error(err, "failed to create ConfigMap")
	}

	log.FromContext(ctx).Info("ConfigMap created successfully", "ConfigMap.Name", configMap.Name)
}

// Manage lifecycle of configmap, delete it once the pod is deleted from the system
func deleteConfigMap(ctx context.Context, k8sClient client.Client, configMapName string, namespace string) error {
	// Define the ConfigMap object with the name and namespace
	configMap := &v1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapName,
			Namespace: namespace,
		},
	}

	err := k8sClient.Delete(ctx, configMap)
	if err != nil {
		log.FromContext(ctx).Error(err, "Failed to delete ConfigMap")
		return err
	}
	fmt.Printf("ConfigMap deleted successfully %v", configMapName)
	return nil
}

func createPatchData(resourceName string, resourceValue string) ([]byte, error) {
	patch := []ResPatchOperation{
		{Op: "add",
			Path:  fmt.Sprintf("/status/capacity/%s", strings.ReplaceAll(resourceName, "/", "~1")),
			Value: resourceValue,
		},
	}
	return json.Marshal(patch)
}

func deletePatchData(resourceName string) ([]byte, error) {
	patch := []ResPatchOperation{
		{Op: "remove",
			Path: fmt.Sprintf("/status/capacity/%s", strings.ReplaceAll("org.instaslice/"+resourceName, "/", "~1")),
		},
	}
	return json.Marshal(patch)
}
