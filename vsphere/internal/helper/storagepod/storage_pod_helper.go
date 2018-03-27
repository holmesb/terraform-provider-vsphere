package storagepod

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/terraform-providers/terraform-provider-vsphere/vsphere/internal/helper/folder"
	"github.com/terraform-providers/terraform-provider-vsphere/vsphere/internal/helper/provider"
	"github.com/terraform-providers/terraform-provider-vsphere/vsphere/internal/helper/virtualmachine"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// FromID locates a StoragePod by its managed object reference ID.
func FromID(client *govmomi.Client, id string) (*object.StoragePod, error) {
	log.Printf("[DEBUG] Locating datastore cluster with ID %q", id)
	finder := find.NewFinder(client.Client, false)

	ref := types.ManagedObjectReference{
		Type:  "StoragePod",
		Value: id,
	}

	ctx, cancel := context.WithTimeout(context.Background(), provider.DefaultAPITimeout)
	defer cancel()
	r, err := finder.ObjectReference(ctx, ref)
	if err != nil {
		return nil, err
	}
	pod := r.(*object.StoragePod)
	log.Printf("[DEBUG] Datastore cluster with ID %q found (%s)", pod.Reference().Value, pod.InventoryPath)
	return pod, nil
}

// FromPath loads a StoragePod from its path. The datacenter is optional if the
// path is specific enough to not require it.
func FromPath(client *govmomi.Client, name string, dc *object.Datacenter) (*object.StoragePod, error) {
	finder := find.NewFinder(client.Client, false)
	if dc != nil {
		log.Printf("[DEBUG] Attempting to locate datastore cluster %q in datacenter %q", name, dc.InventoryPath)
		finder.SetDatacenter(dc)
	} else {
		log.Printf("[DEBUG] Attempting to locate datastore cluster at absolute path %q", name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), provider.DefaultAPITimeout)
	defer cancel()
	return finder.DatastoreCluster(ctx, name)
}

// Properties is a convenience method that wraps fetching the
// StoragePod MO from its higher-level object.
func Properties(pod *object.StoragePod) (*mo.StoragePod, error) {
	ctx, cancel := context.WithTimeout(context.Background(), provider.DefaultAPITimeout)
	defer cancel()
	var props mo.StoragePod
	if err := pod.Properties(ctx, pod.Reference(), nil, &props); err != nil {
		return nil, err
	}
	return &props, nil
}

// Create creates a StoragePod from a supplied folder. The resulting StoragePod
// is returned.
func Create(f *object.Folder, name string) (*object.StoragePod, error) {
	log.Printf("[DEBUG] Creating datastore cluster %q", fmt.Sprintf("%s/%s", f.InventoryPath, name))
	ctx, cancel := context.WithTimeout(context.Background(), provider.DefaultAPITimeout)
	defer cancel()
	pod, err := f.CreateStoragePod(ctx, name)
	if err != nil {
		return nil, err
	}
	return pod, nil
}

// ApplyDRSConfiguration takes a types.StorageDrsConfigSpec and applies it
// against the specified StoragePod.
func ApplyDRSConfiguration(client *govmomi.Client, pod *object.StoragePod, spec types.StorageDrsConfigSpec) error {
	log.Printf("[DEBUG] Applying storage DRS configuration against datastore clsuter %q", pod.InventoryPath)
	mgr := object.NewStorageResourceManager(client.Client)
	ctx, cancel := context.WithTimeout(context.Background(), provider.DefaultAPITimeout)
	defer cancel()
	task, err := mgr.ConfigureStorageDrsForPod(ctx, pod, spec, true)
	if err != nil {
		return err
	}
	return task.Wait(ctx)
}

// Rename renames a StoragePod.
func Rename(pod *object.StoragePod, name string) error {
	log.Printf("[DEBUG] Renaming storage pod %q to %s", pod.InventoryPath, name)
	ctx, cancel := context.WithTimeout(context.Background(), provider.DefaultAPITimeout)
	defer cancel()
	task, err := pod.Rename(ctx, name)
	if err != nil {
		return err
	}
	return task.Wait(ctx)
}

// MoveToFolder is a complex method that moves a StoragePod to a given relative
// datastore folder path. "Relative" here means relative to a datacenter, which
// is discovered from the current StoragePod path.
func MoveToFolder(client *govmomi.Client, pod *object.StoragePod, relative string) error {
	f, err := folder.DatastoreFolderFromObject(client, pod, relative)
	if err != nil {
		return err
	}
	return folder.MoveObjectTo(pod.Reference(), f)
}

// HasChildren checks to see if a datastore cluster has any child items
// (datastores) and returns true if that is the case. This is useful when
// checking to see if a datastore cluster is safe to delete - destroying a
// datastore cluster in vSphere destroys *all* children if at all possible
// (including removing datastores), so extra verification is necessary to
// prevent accidental removal.
func HasChildren(pod *object.StoragePod) (bool, error) {
	return folder.HasChildren(pod.Folder)
}

// Delete destroys a StoragePod.
func Delete(pod *object.StoragePod) error {
	log.Printf("[DEBUG] Deleting datastore cluster %q", pod.InventoryPath)
	ctx, cancel := context.WithTimeout(context.Background(), provider.DefaultAPITimeout)
	defer cancel()
	task, err := pod.Destroy(ctx)
	if err != nil {
		return err
	}
	return task.Wait(ctx)
}

// StorageDRSEnabled checks a StoragePod to see if Storage DRS is enabled.
func StorageDRSEnabled(pod *object.StoragePod) (bool, error) {
	props, err := Properties(pod)
	if err != nil {
		return false, err
	}
	if props.PodStorageDrsEntry == nil {
		return false, nil
	}
	return props.PodStorageDrsEntry.StorageDrsConfig.PodConfig.Enabled, nil
}

// CreateVM creates a virtual machine on a datastore cluster via the
// StorageResourceManager API. It mimics our helper in the virtualmachine
// package in functionality, returning a VM helper object on success.
func CreateVM(
	client *govmomi.Client,
	fo *object.Folder,
	spec types.VirtualMachineConfigSpec,
	pool *object.ResourcePool,
	host *object.HostSystem,
	pod *object.StoragePod,
) (*object.VirtualMachine, error) {
	sdrsEnabled, err := StorageDRSEnabled(pod)
	if err != nil {
		return nil, err
	}
	if !sdrsEnabled {
		return nil, fmt.Errorf("storage DRS is not enabled on datastore cluster %q", pod.Name())
	}
	log.Printf(
		"[DEBUG] Creating virtual machine %q on datastore cluster %q",
		fmt.Sprintf("%s/%s", fo.InventoryPath, spec.Name),
		pod.Name(),
	)

	sps := types.StoragePlacementSpec{
		Type:         string(types.StoragePlacementSpecPlacementTypeCreate),
		ResourcePool: types.NewReference(pool.Reference()),
		PodSelectionSpec: types.StorageDrsPodSelectionSpec{
			StoragePod:      types.NewReference(pod.Reference()),
			InitialVmConfig: expandVMPodConfigForPlacement(spec.DeviceChange, pod),
		},
		ConfigSpec: &spec,
		Folder:     types.NewReference(fo.Reference()),
	}
	if host != nil {
		sps.Host = types.NewReference(host.Reference())
	}

	return recommendAndApplySDRS(client, sps)
}

// CloneVM clones a virtual machine to a datastore cluster via the
// StorageResourceManager API. It mimics our helper in the virtualmachine
// package in functionality, returning a VM helper object on success.
func CloneVM(
	client *govmomi.Client,
	src *object.VirtualMachine,
	fo *object.Folder,
	name string,
	spec types.VirtualMachineCloneSpec,
	timeout int,
	pod *object.StoragePod,
) (*object.VirtualMachine, error) {
	sdrsEnabled, err := StorageDRSEnabled(pod)
	if err != nil {
		return nil, err
	}
	if !sdrsEnabled {
		return nil, fmt.Errorf("storage DRS is not enabled on datastore cluster %q", pod.Name())
	}
	log.Printf(
		"[DEBUG] Cloning virtual machine %q to %q on datastore cluster %q",
		src.InventoryPath,
		fmt.Sprintf("%s/%s", fo.InventoryPath, name),
		pod.Name(),
	)

	sps := types.StoragePlacementSpec{
		Folder:    types.NewReference(fo.Reference()),
		Vm:        types.NewReference(src.Reference()),
		CloneName: name,
		CloneSpec: &spec,
		PodSelectionSpec: types.StorageDrsPodSelectionSpec{
			StoragePod: types.NewReference(pod.Reference()),
		},
		Type: string(types.StoragePlacementSpecPlacementTypeClone),
	}

	return recommendAndApplySDRS(client, sps)
}

// ReconfigureVM reconfigures a virtual machine via the StorageResourceManager
// API, applying any disk modifications that will require going through Storage
// DRS. It mimics our helper in the virtualmachine package in functionality,
// and actually defers to its counterpart in the event there are no disk
// operations.
func ReconfigureVM(
	client *govmomi.Client,
	vm *object.VirtualMachine,
	spec types.VirtualMachineConfigSpec,
	pod *object.StoragePod,
) error {
	sdrsEnabled, err := StorageDRSEnabled(pod)
	if err != nil {
		return err
	}
	if !sdrsEnabled {
		return fmt.Errorf("storage DRS is not enabled on datastore cluster %q", pod.Name())
	}

	// Expand the initialVmConfig field now and check to see if we actually have
	// operations. If we don't, just defer to the standard reconfigure method.
	initialVMConfigs := expandVMPodConfigForPlacement(spec.DeviceChange, pod)
	if len(initialVMConfigs) < 1 {
		log.Printf("[DEBUG] No disk operations for reconfiguration of VM %q, deferring to standard API", vm.InventoryPath)
		return virtualmachine.Reconfigure(vm, spec)
	}

	log.Printf(
		"[DEBUG] Reconfiguring virtual machine %q through Storage DRS API, on datastore cluster %q",
		vm.InventoryPath,
		pod.Name(),
	)

	sps := types.StoragePlacementSpec{
		Type: string(types.StoragePlacementSpecPlacementTypeReconfigure),
		PodSelectionSpec: types.StorageDrsPodSelectionSpec{
			InitialVmConfig: initialVMConfigs,
		},
		Vm:         types.NewReference(vm.Reference()),
		ConfigSpec: &spec,
	}

	_, err = recommendAndApplySDRS(client, sps)
	return err
}

func recommendAndApplySDRS(client *govmomi.Client, sps types.StoragePlacementSpec) (*object.VirtualMachine, error) {
	log.Println("[DEBUG] Acquiring and applying Storage DRS recommendations")
	srm := object.NewStorageResourceManager(client.Client)
	ctx, cancel := context.WithTimeout(context.Background(), provider.DefaultAPITimeout)
	defer cancel()
	placement, err := srm.RecommendDatastores(ctx, sps)
	if err != nil {
		return nil, err
	}

	if len(placement.Recommendations) < 1 {
		return nil, errors.New("no storage DRS recommendations were found for the requested operation")
	}

	// Apply the first recommendation
	task, err := srm.ApplyStorageDrsRecommendation(ctx, []string{placement.Recommendations[0].Key})
	if err != nil {
		return nil, err
	}
	result, err := task.WaitForResult(ctx, nil)
	if err != nil {
		return nil, err
	}

	// If the outer caller was for an operation that could produce a virtual
	// machine, we want to return a full helper object. Check the result and
	// fetch the VM if a reference exists.
	var vm *object.VirtualMachine
	vmRef := result.Result.(types.ApplyStorageRecommendationResult).Vm
	if vmRef != nil {
		log.Printf("[DEBUG] Storage DRS operation returned virtual machine reference: %s", vmRef)
		vm, err = virtualmachine.FromMOID(client, vmRef.Value)
		if err != nil {
			return nil, err
		}
	}
	return vm, nil
}

func expandVMPodConfigForPlacement(dc []types.BaseVirtualDeviceConfigSpec, pod *object.StoragePod) []types.VmPodConfigForPlacement {
	var initialVMConfig []types.VmPodConfigForPlacement

	for _, deviceConfigSpec := range dc {
		s := deviceConfigSpec.GetVirtualDeviceConfigSpec()
		if s.Operation != types.VirtualDeviceConfigSpecOperationAdd {
			continue
		}

		if s.FileOperation != types.VirtualDeviceConfigSpecFileOperationCreate {
			continue
		}

		d, ok := s.Device.(*types.VirtualDisk)
		if !ok {
			continue
		}

		podConfigForPlacement := types.VmPodConfigForPlacement{
			StoragePod: pod.Reference(),
			Disk: []types.PodDiskLocator{
				{
					DiskId:          d.Key,
					DiskBackingInfo: d.Backing,
				},
			},
		}

		initialVMConfig = append(initialVMConfig, podConfigForPlacement)
	}

	return initialVMConfig
}
